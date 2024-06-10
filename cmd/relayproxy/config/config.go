package config

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
	"github.com/xitongsys/parquet-go/parquet"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var k = koanf.New(".")
var DefaultRetriever = struct {
	Timeout    time.Duration
	HTTPMethod string
	GitBranch  string
}{
	Timeout:    10 * time.Second,
	HTTPMethod: http.MethodGet,
	GitBranch:  "main",
}

const DefaultLogLevel = "info"

var DefaultExporter = struct {
	Format                  string
	LogFormat               string
	FileName                string
	CsvFormat               string
	FlushInterval           time.Duration
	MaxEventInMemory        int64
	ParquetCompressionCodec string
	LogLevel                string
}{
	Format:    "JSON",
	LogFormat: "[{{ .FormattedDate}}] user=\"{{ .UserKey}}\", flag=\"{{ .Key}}\", value=\"{{ .Value}}\"",
	FileName:  "flag-variation-{{ .Hostname}}-{{ .Timestamp}}.{{ .Format}}",
	CsvFormat: "{{ .Kind}};{{ .ContextKind}};{{ .UserKey}};{{ .CreationDate}};{{ .Key}};{{ .Variation}};" +
		"{{ .Value}};{{ .Default}};{{ .Source}}\n",
	FlushInterval:           60000 * time.Millisecond,
	MaxEventInMemory:        100000,
	ParquetCompressionCodec: parquet.CompressionCodec_SNAPPY.String(),
	LogLevel:                DefaultLogLevel,
}

// New is reading the configuration file
func New(flagSet *pflag.FlagSet, log *zap.Logger, version string) (*Config, error) {
	k.Delete("")

	// Default values
	_ = k.Load(confmap.Provider(map[string]interface{}{
		"listen":          "1031",
		"host":            "localhost",
		"fileFormat":      "yaml",
		"restApiTimeout":  5000,
		"pollingInterval": 60000,
		"logLevel":        DefaultLogLevel,
	}, "."), nil)

	// mapping command line parameters to koanf
	if errBindFlag := k.Load(posflag.Provider(flagSet, ".", k), nil); errBindFlag != nil {
		log.Fatal("impossible to parse flag command line", zap.Error(errBindFlag))
	}

	// Read config file
	configFileLocation, errFileLocation := locateConfigFile(k.String("config"))
	if errFileLocation != nil {
		log.Info("not using any configuration file", zap.Error(errFileLocation))
	} else {
		ext := filepath.Ext(configFileLocation)
		var parser koanf.Parser
		switch strings.ToLower(ext) {
		case ".toml":
			parser = toml.Parser()
			break
		case ".json":
			parser = json.Parser()
			break
		default:
			parser = yaml.Parser()
		}

		if errBindFile := k.Load(file.Provider(configFileLocation), parser); errBindFile != nil {
			log.Error("error loading file", zap.Error(errBindFile))
		}
	}
	// Map environment variables
	_ = k.Load(env.ProviderWithValue("", ".", func(s string, v string) (string, interface{}) {
		if strings.HasPrefix(s, "RETRIEVERS") || strings.HasPrefix(s, "NOTIFIERS") {
			configMap := k.Raw()
			err := loadArrayEnv(s, v, configMap)
			if err != nil {
				log.Error("config: error loading array env", zap.String("key", s), zap.String("value", v), zap.Error(err))
				return s, v
			}
			return s, v
		}
		return strings.ReplaceAll(strings.ToLower(s), "_", "."), v
	}), nil)

	_ = k.Set("version", version)

	proxyConf := &Config{}
	errUnmarshal := k.Unmarshal("", &proxyConf)
	if errUnmarshal != nil {
		return nil, errUnmarshal
	}

	if proxyConf.Debug && proxyConf.LogLevel == "" {
		log.Warn(
			"Option Debug that you are using in your configuration file is deprecated" +
				"and will be removed in future versions." +
				"Please use logLevel: debug to continue to run the relay-proxy with debug logs.")
	}

	return proxyConf, nil
}

type Config struct {
	// ListenPort (optional) is the port we are using to start the proxy
	ListenPort int `mapstructure:"listen" koanf:"listen"`

	// HideBanner (optional) if true, we don't display the go-feature-flag relay proxy banner
	HideBanner bool `mapstructure:"hideBanner" koanf:"hidebanner"`

	// Debug (optional) if true, go-feature-flag relay proxy will run on debug mode, with more logs and custom responses
	Debug bool `mapstructure:"debug" koanf:"debug"`

	// EnableSwagger (optional) to have access to the swagger
	EnableSwagger bool `mapstructure:"enableSwagger" koanf:"enableswagger"`

	// Host should be set if you are using swagger (default is localhost)
	Host string `mapstructure:"host" koanf:"host"`

	// LogLevel (optional) sets the verbosity for logging,
	// Possible values: debug, info, warn, error, dpanic, panic, fatal
	// If level debug go-feature-flag relay proxy will run on debug mode, with more logs and custom responses
	// Default: debug
	LogLevel string `mapstructure:"logLevel" koanf:"loglevel"`

	// PollingInterval (optional) Poll every X time
	// The minimum possible is 1 second
	// Default: 60 seconds
	PollingInterval int `mapstructure:"pollingInterval" koanf:"pollinginterval"`

	// EnablePollingJitter (optional) set to true if you want to avoid having true periodicity when
	// retrieving your flags. It is useful to avoid having spike on your flag configuration storage
	// in case your application is starting multiple instance at the same time.
	// We ensure a deviation that is maximum + or - 10% of your polling interval.
	// Default: false
	EnablePollingJitter bool `mapstructure:"enablePollingJitter" koanf:"enablepollingjitter"`

	// FileFormat (optional) is the format of the file to retrieve (available YAML, TOML and JSON)
	// Default: YAML
	FileFormat string `mapstructure:"fileFormat" koanf:"fileformat"`

	// StartWithRetrieverError (optional) If true, the relay proxy will start even if we did not get any flags from
	// the retriever. It will serve only default values until the retriever returns the flags.
	// The init method will not return any error if the flag file is unreachable.
	// Default: false
	StartWithRetrieverError bool `mapstructure:"startWithRetrieverError" koanf:"startwithretrievererror"`

	// Retriever is the configuration on how to retrieve the file
	Retriever *RetrieverConf `mapstructure:"retriever" koanf:"retriever"`

	// Retrievers is the exact same things than Retriever but allows to give more than 1 retriever at the time.
	// We are dealing with config files in order, if you have the same flag name in multiple files it will be override
	// based of the order of the retrievers in the slice.
	//
	// Note: If both Retriever and Retrievers are set, we will start by calling the Retriever and,
	// after we will use the order of Retrievers.
	Retrievers *[]RetrieverConf `mapstructure:"retrievers" koanf:"retrievers"`

	// Exporter is the configuration on how to export data
	Exporter *ExporterConf `mapstructure:"exporter" koanf:"exporter"`

	// Notifiers is the configuration on where to notify a flag change
	Notifiers []NotifierConf `mapstructure:"notifier" koanf:"notifier"`

	// RestAPITimeout is the timeout on the API.
	RestAPITimeout int `mapstructure:"restApiTimeout" koanf:"restapitimeout"`

	// Version is the version of the relay-proxy
	Version string `mapstructure:"version" koanf:"version"`

	// Deprecated: use AuthorizedKeys instead
	// APIKeys list of API keys that authorized to use endpoints
	APIKeys []string `mapstructure:"apiKeys" koanf:"apikeys"`

	// AuthorizedKeys list of API keys that authorized to use endpoints
	AuthorizedKeys APIKeys `mapstructure:"authorizedKeys" koanf:"authorizedkeys"`

	// StartAsAwsLambda (optional) if true, the relay proxy will start ready to be launched as AWS Lambda
	StartAsAwsLambda bool `mapstructure:"startAsAwsLambda" koanf:"startasawslambda"`

	// EvaluationContextEnrichment (optional) will be merged with the evaluation context sent during the evaluation.
	// It is useful to add common attributes to all the evaluations, such as a server version, environment, ...
	//
	// All those fields will be included in the custom attributes of the evaluation context,
	// if in the evaluation context you have a field with the same name,
	// it will be overridden by the evaluationContextEnrichment.
	// Default: nil
	EvaluationContextEnrichment map[string]interface{} `mapstructure:"evaluationContextEnrichment" koanf:"evaluationcontextenrichment"` //nolint: lll

	// OpenTelemetryOtlpEndpoint (optional) is the endpoint of the OpenTelemetry collector
	// Default: ""
	OpenTelemetryOtlpEndpoint string `mapstructure:"openTelemetryOtlpEndpoint" koanf:"opentelemetryotlpendpoint"`

	// MonitoringPort (optional) is the port we are using to expose the metrics and healthchecks
	// If not set we will use the same port as the proxy
	MonitoringPort int `mapstructure:"monitoringPort" koanf:"monitoringport"`

	// ---- private fields

	// apiKeySet is the internal representation of an API keys list configured
	// we store them in a set to be
	apiKeysSet map[string]interface{}

	// adminAPIKeySet is the internal representation of an admin API keys list configured
	// we store them in a set to be
	adminAPIKeySet map[string]interface{}
}

// APIKeysAdminExists is checking if an admin API Key exist in the relay proxy configuration
func (c *Config) APIKeysAdminExists(apiKey string) bool {
	if c.adminAPIKeySet == nil {
		adminAPIKeySet := make(map[string]interface{})
		for _, currentAPIKey := range c.AuthorizedKeys.Admin {
			adminAPIKeySet[currentAPIKey] = new(interface{})
		}
		c.adminAPIKeySet = adminAPIKeySet
	}

	_, ok := c.adminAPIKeySet[apiKey]
	return ok
}

// APIKeyExists is checking if an API Key exist in the relay proxy configuration
func (c *Config) APIKeyExists(apiKey string) bool {
	if c.APIKeysAdminExists(apiKey) {
		return true
	}
	if c.apiKeysSet == nil {
		apiKeySet := make(map[string]interface{})

		// Remove this part when the APIKeys field is removed
		for _, currentAPIKey := range c.APIKeys {
			apiKeySet[currentAPIKey] = new(interface{})
		}
		// end of remove

		for _, currentAPIKey := range c.AuthorizedKeys.Evaluation {
			apiKeySet[currentAPIKey] = new(interface{})
		}
		c.apiKeysSet = apiKeySet
	}

	_, ok := c.apiKeysSet[apiKey]
	return ok
}

// IsValid contains all the validation of the configuration.
func (c *Config) IsValid() error {
	if c == nil {
		return fmt.Errorf("empty config")
	}

	if c.ListenPort == 0 {
		return fmt.Errorf("invalid port %d", c.ListenPort)
	}

	if c.Retriever == nil && c.Retrievers == nil {
		return fmt.Errorf("no retriever available in the configuration")
	}

	if c.Retriever != nil {
		if err := c.Retriever.IsValid(); err != nil {
			return err
		}
	}

	if c.Retrievers != nil {
		for _, retriever := range *c.Retrievers {
			if err := retriever.IsValid(); err != nil {
				return err
			}
		}
	}

	// Exporter is optional
	if c.Exporter != nil {
		if err := c.Exporter.IsValid(); err != nil {
			return err
		}
	}

	if c.Notifiers != nil {
		for _, notif := range c.Notifiers {
			if err := notif.IsValid(); err != nil {
				return err
			}
		}
	}
	if c.LogLevel != "" {
		if _, err := zapcore.ParseLevel(c.LogLevel); err != nil {
			return err
		}
	}

	return nil
}

// locateConfigFile is selecting the configuration file we will use.
func locateConfigFile(inputFilePath string) (string, error) {
	filename := "goff-proxy"
	defaultLocations := []string{
		"./",
		"/goff/",
		"/etc/opt/goff/",
	}
	supportedExtensions := []string{
		"yaml",
		"toml",
		"json",
		"yml",
	}

	if inputFilePath != "" {
		if _, err := os.Stat(inputFilePath); err != nil {
			return "", fmt.Errorf("impossible to find config file %s", inputFilePath)
		}
		return inputFilePath, nil
	}
	for _, location := range defaultLocations {
		for _, ext := range supportedExtensions {
			configFile := fmt.Sprintf("%s%s.%s", location, filename, ext)
			if _, err := os.Stat(configFile); err == nil {
				return configFile, nil
			}
		}
	}
	return "", fmt.Errorf(
		"impossible to find config file in the default locations [%s]", strings.Join(defaultLocations, ","))
}

// Load the ENV Like:RETRIEVERS_0_HEADERS_AUTHORIZATION
func loadArrayEnv(s string, v string, configMap map[string]interface{}) error {
	paths := strings.Split(s, "_")
	for i, str := range paths {
		paths[i] = strings.ToLower(str)
	}
	prefixKey := paths[0]
	if configArray, ok := configMap[prefixKey].([]interface{}); ok {
		index, err := strconv.Atoi(paths[1])
		if err != nil {
			return err
		}
		var configItem map[string]interface{}
		outRange := index > len(configArray)-1
		if outRange {
			configItem = make(map[string]interface{})
		} else {
			configItem = configArray[index].(map[string]interface{})
		}

		keys := paths[2:]
		currentMap := configItem
		for i, key := range keys {
			hasKey := false
			lowerKey := key
			for y := range currentMap {
				if y != lowerKey {
					continue
				}
				if nextMap, ok := currentMap[y].(map[string]interface{}); ok {
					currentMap = nextMap
					hasKey = true
					break
				}
			}
			if !hasKey && i != len(keys)-1 {
				newMap := make(map[string]interface{})
				currentMap[lowerKey] = newMap
				currentMap = newMap
			}
		}
		lastKey := keys[len(keys)-1]
		currentMap[lastKey] = v
		if outRange {
			blank := index - len(configArray) + 1
			for i := 0; i < blank; i++ {
				configArray = append(configArray, make(map[string]interface{}))
			}
			configArray[index] = configItem
		} else {
			configArray[index] = configItem
		}
		_ = k.Set(prefixKey, configArray)
	}
	return nil
}

func (c *Config) IsDebugEnabled() bool {
	if c == nil {
		return false
	}
	return c.LogLevel == "debug" || c.Debug
}

func (c *Config) ZapLogLevel() zapcore.Level {
	if c == nil {
		return zapcore.InvalidLevel
	}
	// Use debug flag for backward compatibility
	if c.Debug {
		return zapcore.DebugLevel
	}

	level, err := zapcore.ParseLevel(c.LogLevel)
	if err != nil {
		return zapcore.InvalidLevel
	}
	return level
}
