package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	prowflagutil "k8s.io/test-infra/prow/flagutil"
	"k8s.io/test-infra/prow/github"
	"log"
	"net/url"
	"os"
	"sync"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/test-infra/pkg/flagutil"

	"gopkg.in/yaml.v2"

	"github.com/spf13/pflag"

	citools "github.com/openshift/ci-tools/pkg/api"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog"
	configflagutil "k8s.io/test-infra/prow/flagutil/config"

	imageclientset "github.com/openshift/client-go/image/clientset/versioned"
	projectclientset "github.com/openshift/client-go/project/clientset/versioned"
)

type options struct {
	prowconfig               configflagutil.ConfigOptions
	GitHubOptions            prowflagutil.GitHubOptions
	KubernetesOptions        prowflagutil.KubernetesOptions
	ForcePROwner             string
	ReleaseClusterKubeconfig string
	ConfigResolver           string
	WorkflowConfigPath       string
	Port                     int
	GracePeriod              time.Duration
}

func (o *options) Validate() error {
	if o.ReleaseClusterKubeconfig != "" {
		if _, err := os.Stat(o.ReleaseClusterKubeconfig); err != nil {
			return fmt.Errorf("error accessing --release-cluster-kubeconfig: %w", err)
		}
	}
	for _, group := range []flagutil.OptionGroup{&o.GitHubOptions, &o.KubernetesOptions} {
		if err := group.Validate(false); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	emptyFlags := flag.NewFlagSet("empty", flag.ContinueOnError)
	klog.InitFlags(emptyFlags)
	opt := &options{
		prowconfig: configflagutil.ConfigOptions{
			ConfigPathFlagName:    "prow-config",
			JobConfigPathFlagName: "job-config",
		},
		ConfigResolver:    "http://config.ci.openshift.org/config",
		KubernetesOptions: prowflagutil.KubernetesOptions{NOInClusterConfigDefault: true},
	}

	pflag.StringVar(&opt.ConfigResolver, "config-resolver", opt.ConfigResolver, "A URL pointing to a config resolver for retrieving ci-operator config. You may pass a location on disk with file://<abs_path_to_ci_operator_config>")
	pflag.StringVar(&opt.ForcePROwner, "force-pr-owner", opt.ForcePROwner, "Make the supplied user the owner of all PRs for access control purposes.")
	pflag.StringVar(&opt.ReleaseClusterKubeconfig, "release-cluster-kubeconfig", "", "Kubeconfig to use for cluster housing the release imagestreams. Defaults to normal kubeconfig if unset.")
	pflag.StringVar(&opt.WorkflowConfigPath, "workflow-config-path", "", "Path to config file used for workflow commands")
	pflag.IntVar(&opt.Port, "port", 8080, "Port to listen on.")
	pflag.DurationVar(&opt.GracePeriod, "grace-period", 5*time.Second, "On shutdown, try to handle remaining events for the specified duration.")

	opt.prowconfig.AddFlags(emptyFlags)
	opt.GitHubOptions.AddFlags(emptyFlags)
	opt.KubernetesOptions.AddFlags(emptyFlags)
	pflag.CommandLine.AddGoFlagSet(emptyFlags)
	pflag.Parse()
	klog.SetOutput(os.Stderr)

	if err := opt.Validate(); err != nil {
		return fmt.Errorf("unable to validate program arguments: %v", err)
	}

	err := opt.KubernetesOptions.AddKubeconfigChangeCallback(func() {
		klog.Infof("received kubeconfig changed event, exiting to make the kubelet restart us so we can pick them up")
		os.Exit(0)
	})
	if err != nil {
		return fmt.Errorf("failed to set up kubeconfig watches: %v", err)
	}
	kubeConfigs, err := opt.KubernetesOptions.LoadClusterConfigs()
	if err != nil {
		return fmt.Errorf("could not load kube configs: %v", err)
	}
	buildClusterClientConfigs, err := processKubeConfigs(kubeConfigs)
	if err != nil {
		return fmt.Errorf("could not process kube configs: %v", err)
	}

	resolverURL, err := url.Parse(opt.ConfigResolver)
	if err != nil {
		return fmt.Errorf("--config-resolver is not a valid URL: %v", err)
	}
	resolver := &URLConfigResolver{URL: resolverURL}

	botToken := os.Getenv("BOT_TOKEN")
	if len(botToken) == 0 {
		return fmt.Errorf("the environment variable BOT_TOKEN must be set")
	}

	botSigningSecret := os.Getenv("BOT_SIGNING_SECRET")
	if len(botSigningSecret) == 0 {
		return fmt.Errorf("the environment variable BOT_SIGNING_SECRET must be set")
	}

	prowJobKubeconfig, _, _, err := loadKubeconfig()
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(prowJobKubeconfig)
	if err != nil {
		return fmt.Errorf("unable to create prow client: %v", err)
	}
	prowClient := dynamicClient.Resource(schema.GroupVersionResource{Group: "prow.k8s.io", Version: "v1", Resource: "prowjobs"})

	// Config and Client to access release images
	releaseConfig, err := loadKubeconfigFromFlagOrDefault(opt.ReleaseClusterKubeconfig, prowJobKubeconfig)
	imageClient, err := imageclientset.NewForConfig(releaseConfig)
	if err != nil {
		return fmt.Errorf("unable to create image client: %v", err)
	}

	configAgent, err := opt.prowconfig.ConfigAgent()
	if err != nil {
		return err
	}

	workflows := WorkflowConfig{}
	go manageWorkflowConfig(opt.WorkflowConfigPath, &workflows)

	var ghClient github.Client

	if token := os.Getenv("GITHUB_TOKEN"); len(token) > 0 {
		ghClient, err = opt.GitHubOptions.GitHubClientWithAccessToken(token)
		if err != nil {
			return fmt.Errorf("unable to create github client: %v", err)
		}
		_, err := ghClient.BotUser()
		if err != nil {
			return fmt.Errorf("unable to get github bot user: %v", err)
		}
	} else {
		ghClient, err = opt.GitHubOptions.GitHubClient(false)
		if err != nil {
			return fmt.Errorf("unable to create github client: %v", err)
		}
	}

	manager := NewJobManager(configAgent, resolver, prowClient, imageClient, buildClusterClientConfigs, ghClient, opt.ForcePROwner, &workflows)
	if err := manager.Start(); err != nil {
		return fmt.Errorf("unable to load initial configuration: %v", err)
	}

	bot := NewBot(botToken, botSigningSecret, opt.GracePeriod, opt.Port, &workflows)
	bot.Start(manager)

	return err
}

type BuildClusterClientConfig struct {
	CoreConfig        *rest.Config
	CoreClient        *clientset.Clientset
	ProjectClient     *projectclientset.Clientset
	TargetImageClient *imageclientset.Clientset
}

type BuildClusterClientConfigMap map[string]*BuildClusterClientConfig

func processKubeConfigs(kubeConfigs map[string]rest.Config) (BuildClusterClientConfigMap, error) {
	clusterMap := make(BuildClusterClientConfigMap)
	for clusterName, clusterConfig := range kubeConfigs {
		tmpClusterConfig := clusterConfig
		coreClient, err := clientset.NewForConfig(&tmpClusterConfig)
		if err != nil {
			return nil, fmt.Errorf("unable to create core client: %v", err)
		}
		targetImageClient, err := imageclientset.NewForConfig(&tmpClusterConfig)
		if err != nil {
			return nil, fmt.Errorf("unable to create target image client: %w", err)
		}
		projectClient, err := projectclientset.NewForConfig(&tmpClusterConfig)
		if err != nil {
			return nil, fmt.Errorf("unable to create project client: %v", err)
		}
		clusterMap[clusterName] = &BuildClusterClientConfig{
			CoreConfig:        &tmpClusterConfig,
			CoreClient:        coreClient,
			ProjectClient:     projectClient,
			TargetImageClient: targetImageClient,
		}
	}
	return clusterMap, nil
}

type WorkflowConfig struct {
	Workflows map[string]WorkflowConfigItem `yaml:"workflows"`
	mutex     sync.RWMutex                  `yaml:"-"` // this field just allows us to update the above values without races
}

type WorkflowConfigItem struct {
	BaseImages   map[string]citools.ImageStreamTagReference `yaml:"base_images,omitempty"`
	Architecture string                                     `yaml:"architecture,omitempty"`
	Platform     string                                     `yaml:"platform"`
}

func manageWorkflowConfig(path string, workflows *WorkflowConfig) {
	for {
		// To prevent the ci-chat-bot from crashlooping due to a bad config change,
		// we will only log that the config is broken and set the workflows to an
		// empty map. To prevent broken configs in the future, a presubmit should
		// be creating for openshift/release that verifies this config.
		var config WorkflowConfig
		rawConfig, err := ioutil.ReadFile(path)
		if err != nil {
			klog.Errorf("Failed to load workflow config file at %s: %v", path, err)
		} else if err := yaml.Unmarshal(rawConfig, &config); err != nil {
			klog.Errorf("Failed to unmarshal workflow config: %v", err)
		}

		workflows.mutex.Lock()
		if config.Workflows != nil {
			workflows.Workflows = config.Workflows
		} else {
			workflows.Workflows = make(map[string]WorkflowConfigItem)
		}
		workflows.mutex.Unlock()
		time.Sleep(2 * time.Minute)
	}
}
