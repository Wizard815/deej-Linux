package deej

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"github.com/thoas/go-funk"
	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

// CanonicalConfig provides application-wide access to configuration fields,
// as well as loading/file watching logic for deej's configuration file
type CanonicalConfig struct {
	SliderMapping *sliderMap

	ConnectionInfo struct {
		COMPort  string
		BaudRate int
	}

	InvertSliders bool

	NoiseReductionLevel string

	// the highest raw value the board sends for a slider (1023 for 10-bit arduino boards, 4095 for 12-bit ESP32 boards)
	SliderMaxValue int

	logger             *zap.SugaredLogger
	notifier           Notifier
	stopWatcherChannel chan bool

	reloadConsumers []chan bool

	userConfig     *viper.Viper
	internalConfig *viper.Viper

	// guards the fields above (which change whenever the config reloads) and the editor's copy of the user config,
	// since they're read from other goroutines. use the accessors below rather than reading the fields directly
	lock     sync.Mutex
	editable editableConfig
}

const (
	userConfigFilepath     = "config.yaml"
	internalConfigFilepath = "preferences.yaml"

	userConfigName     = "config"
	internalConfigName = "preferences"

	userConfigPath = "."

	configType = "yaml"

	configKeySliderMapping       = "slider_mapping"
	configKeyInvertSliders       = "invert_sliders"
	configKeyCOMPort             = "com_port"
	configKeyBaudRate            = "baud_rate"
	configKeyNoiseReductionLevel = "noise_reduction"
	configKeySliderMaxValue      = "slider_max_value"

	defaultCOMPort             = util.AutoSerialPort
	defaultBaudRate            = 9600
	defaultSliderMaxValue      = 1023
	defaultNoiseReductionLevel = "default"
)

// has to be defined as a non-constant because we're using path.Join
var internalConfigPath = path.Join(".", logDirectory)

var defaultSliderMapping = func() *sliderMap {
	emptyMap := newSliderMap()
	emptyMap.set(0, []string{masterSessionName})

	return emptyMap
}()

// NewConfig creates a config instance for the deej object and sets up viper instances for deej's config files
func NewConfig(logger *zap.SugaredLogger, notifier Notifier) (*CanonicalConfig, error) {
	logger = logger.Named("config")

	cc := &CanonicalConfig{
		logger:             logger,
		notifier:           notifier,
		reloadConsumers:    []chan bool{},
		stopWatcherChannel: make(chan bool),
	}

	// distinguish between the user-provided config (config.yaml) and the internal config (logs/preferences.yaml)
	userConfig := viper.New()
	userConfig.SetConfigName(userConfigName)
	userConfig.SetConfigType(configType)
	userConfig.AddConfigPath(userConfigPath)

	userConfig.SetDefault(configKeySliderMapping, map[string][]string{})
	userConfig.SetDefault(configKeyInvertSliders, false)
	userConfig.SetDefault(configKeyCOMPort, defaultCOMPort)
	userConfig.SetDefault(configKeyBaudRate, defaultBaudRate)
	userConfig.SetDefault(configKeySliderMaxValue, defaultSliderMaxValue)
	userConfig.SetDefault(configKeyNoiseReductionLevel, defaultNoiseReductionLevel)

	internalConfig := viper.New()
	internalConfig.SetConfigName(internalConfigName)
	internalConfig.SetConfigType(configType)
	internalConfig.AddConfigPath(internalConfigPath)

	cc.userConfig = userConfig
	cc.internalConfig = internalConfig

	logger.Debug("Created config instance")

	return cc, nil
}

// Load reads deej's config files from disk and tries to parse them
func (cc *CanonicalConfig) Load() error {
	cc.logger.Debugw("Loading config", "path", userConfigFilepath)

	// make sure it exists, and create one with the default settings if it doesn't (it can be set up with the slider editor)
	if !util.FileExists(userConfigFilepath) {
		cc.logger.Warnw("Config file not found, creating one with default settings", "path", userConfigFilepath)

		if err := writeConfigFile(defaultEditableConfig()); err != nil {
			cc.logger.Warnw("Failed to create default config file", "error", err)
			cc.notifier.Notify("Can't find configuration!",
				fmt.Sprintf("%s must be in the same directory as deej. Please re-launch", userConfigFilepath))

			return fmt.Errorf("config file doesn't exist: %s", userConfigFilepath)
		}

		cc.notifier.Notify("Created a new configuration",
			"Right-click deej's tray icon and choose \"Open slider editor\" to set up your sliders.")
	}

	// load the user config
	if err := cc.userConfig.ReadInConfig(); err != nil {
		cc.logger.Warnw("Viper failed to read user config", "error", err)

		// if the error is yaml-format-related, show a sensible error. otherwise, show 'em to the logs
		if strings.Contains(err.Error(), "yaml:") {
			cc.notifier.Notify("Invalid configuration!",
				fmt.Sprintf("Please make sure %s is in a valid YAML format.", userConfigFilepath))
		} else {
			cc.notifier.Notify("Error loading configuration!", "Please check deej's logs for more details.")
		}

		return fmt.Errorf("read user config: %w", err)
	}

	// load the internal config - this doesn't have to exist, so it can error
	if err := cc.internalConfig.ReadInConfig(); err != nil {
		cc.logger.Debugw("Viper failed to read internal config", "error", err, "reminder", "this is fine")
	}

	// canonize the configuration with viper's helpers
	if err := cc.populateFromVipers(); err != nil {
		cc.logger.Warnw("Failed to populate config fields", "error", err)
		return fmt.Errorf("populate config fields: %w", err)
	}

	cc.logger.Info("Loaded config successfully")
	cc.logger.Infow("Config values",
		"sliderMapping", cc.SliderMapping,
		"connectionInfo", cc.ConnectionInfo,
		"invertSliders", cc.InvertSliders,
		"sliderMaxValue", cc.SliderMaxValue)

	return nil
}

// SubscribeToChanges allows external components to receive updates when the config is reloaded
func (cc *CanonicalConfig) SubscribeToChanges() chan bool {
	c := make(chan bool)
	cc.reloadConsumers = append(cc.reloadConsumers, c)

	return c
}

// WatchConfigFileChanges starts watching for configuration file changes
// and attempts reloading the config when they happen
func (cc *CanonicalConfig) WatchConfigFileChanges() {
	cc.logger.Debugw("Starting to watch user config file for changes", "path", userConfigFilepath)

	const (
		minTimeBetweenReloadAttempts = time.Millisecond * 500
		delayBetweenEventAndReload   = time.Millisecond * 50
	)

	lastAttemptedReload := time.Now()

	// establish watch using viper as opposed to doing it ourselves, though our internal cooldown is still required.
	// the handler is registered first, since viper's watcher goroutine reads it without synchronization
	cc.userConfig.OnConfigChange(func(event fsnotify.Event) {

		// when we get a write event...
		if event.Op&fsnotify.Write == fsnotify.Write {

			now := time.Now()

			// ... check if it's not a duplicate (many editors will write to a file twice)
			if lastAttemptedReload.Add(minTimeBetweenReloadAttempts).Before(now) {

				// and attempt reload if appropriate
				cc.logger.Debugw("Config file modified, attempting reload", "event", event)

				// wait a bit to let the editor actually flush the new file contents to disk
				<-time.After(delayBetweenEventAndReload)

				if err := cc.Load(); err != nil {
					cc.logger.Warnw("Failed to reload config file", "error", err)
				} else {
					cc.logger.Info("Reloaded config successfully")
					cc.notifier.Notify("Configuration reloaded!", "Your changes have been applied.")

					cc.onConfigReloaded()
				}

				// don't forget to update the time
				lastAttemptedReload = now
			}
		}
	})

	cc.userConfig.WatchConfig()

	// wait till they stop us
	<-cc.stopWatcherChannel
	cc.logger.Debug("Stopping user config file watcher")
	cc.userConfig.OnConfigChange(nil)
}

// StopWatchingConfigFile signals our filesystem watcher to stop
func (cc *CanonicalConfig) StopWatchingConfigFile() {
	cc.stopWatcherChannel <- true
}

func (cc *CanonicalConfig) populateFromVipers() error {

	// merge the slider mappings from the user and internal configs
	sliderMapping := sliderMapFromConfigs(
		cc.userConfig.GetStringMapStringSlice(configKeySliderMapping),
		cc.internalConfig.GetStringMapStringSlice(configKeySliderMapping),
	)

	// get the rest of the config fields - viper saves us a lot of effort here
	comPort := cc.userConfig.GetString(configKeyCOMPort)

	baudRate := cc.userConfig.GetInt(configKeyBaudRate)
	if baudRate <= 0 {
		cc.logger.Warnw("Invalid baud rate specified, using default value",
			"key", configKeyBaudRate,
			"invalidValue", baudRate,
			"defaultValue", defaultBaudRate)

		baudRate = defaultBaudRate
	}

	invertSliders := cc.userConfig.GetBool(configKeyInvertSliders)
	noiseReductionLevel := cc.userConfig.GetString(configKeyNoiseReductionLevel)

	sliderMaxValue := cc.userConfig.GetInt(configKeySliderMaxValue)
	if sliderMaxValue <= 0 {
		cc.logger.Warnw("Invalid slider max value specified, using default value",
			"key", configKeySliderMaxValue,
			"invalidValue", sliderMaxValue,
			"defaultValue", defaultSliderMaxValue)

		sliderMaxValue = defaultSliderMaxValue
	}

	// keep a copy of what's in the user config (without the internal config's mappings) for the slider editor
	editable := editableConfig{
		SliderMapping:  map[int][]string{},
		InvertSliders:  invertSliders,
		COMPort:        comPort,
		BaudRate:       baudRate,
		NoiseReduction: noiseReductionLevel,
		SliderMaxValue: sliderMaxValue,
	}

	for sliderIdxString, targets := range cc.userConfig.GetStringMapStringSlice(configKeySliderMapping) {
		sliderIdx, err := strconv.Atoi(sliderIdxString)
		if err != nil {
			continue
		}

		editable.SliderMapping[sliderIdx] = funk.FilterString(targets, func(s string) bool {
			return s != ""
		})
	}

	// other components read these while the config reloads, so publish them all at once
	cc.lock.Lock()
	cc.SliderMapping = sliderMapping
	cc.ConnectionInfo.COMPort = comPort
	cc.ConnectionInfo.BaudRate = baudRate
	cc.InvertSliders = invertSliders
	cc.NoiseReductionLevel = noiseReductionLevel
	cc.SliderMaxValue = sliderMaxValue
	cc.editable = editable
	cc.lock.Unlock()

	cc.logger.Debug("Populated config fields from vipers")

	return nil
}

func (cc *CanonicalConfig) onConfigReloaded() {
	cc.logger.Debug("Notifying consumers about configuration reload")

	for _, consumer := range cc.reloadConsumers {
		consumer <- true
	}
}

func (cc *CanonicalConfig) sliderMapping() *sliderMap {
	cc.lock.Lock()
	defer cc.lock.Unlock()

	return cc.SliderMapping
}

func (cc *CanonicalConfig) connectionInfo() (comPort string, baudRate int) {
	cc.lock.Lock()
	defer cc.lock.Unlock()

	return cc.ConnectionInfo.COMPort, cc.ConnectionInfo.BaudRate
}

// sliderSettings returns the settings used to turn raw slider values into volumes
func (cc *CanonicalConfig) sliderSettings() (maxValue int, invert bool, noiseReductionLevel string) {
	cc.lock.Lock()
	defer cc.lock.Unlock()

	return cc.SliderMaxValue, cc.InvertSliders, cc.NoiseReductionLevel
}

// editableValues returns a copy of the user config's current values, as shown in the slider editor
func (cc *CanonicalConfig) editableValues() editableConfig {
	cc.lock.Lock()
	defer cc.lock.Unlock()

	result := cc.editable
	result.SliderMapping = make(map[int][]string, len(cc.editable.SliderMapping))

	for sliderIdx, targets := range cc.editable.SliderMapping {
		result.SliderMapping[sliderIdx] = append([]string{}, targets...)
	}

	return result
}
