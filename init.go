package main

import (
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/MicahParks/keyfunc/v2"
	"github.com/fsnotify/fsnotify"
	"github.com/go-sql-driver/mysql"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var (
	Commit              string
	DB                  *sql.DB
	Jwks                *keyfunc.JWKS
	ServiceAccountToken string
	Logger              *zap.Logger
	Cfg                 *Config
	V                   *viper.Viper
	GetLabelsFunc       func(token KeycloakToken) map[string]bool
	atomicLevel         zap.AtomicLevel
)

// init carries out the main initialization routine for the Proxy. It logs the commit information,
// configures the HTTP client to ignore self-signed certificates, reads the service account token,
// initializes JWKS if not in development mode, and establishes a database connection if enabled in the config.
func init() {
	initLogging()
	initConfig()
	updateLogLevel()
	Logger.Info("-------Init Proxy-------")
	Logger.Info("Commit: ", zap.String("commit", Commit))
	Logger.Info("Set http client to ignore self signed certificates")
	Logger.Info("Config ", zap.Any("cfg", Cfg))
	initTLSConfig()
	ServiceAccountToken = Cfg.Dev.ServiceAccountToken
	if !strings.HasSuffix(os.Args[0], ".test") {
		Logger.Debug("Not in test mode")
		initJWKS()
		if !Cfg.Dev.Enabled {
			sa, err := os.ReadFile("/run/secrets/kubernetes.io/serviceaccount/token")
			if err != nil {
				Logger.Panic("Error while reading service account token", zap.Error(err))
			}
			ServiceAccountToken = string(sa)
		}
	} else {
		if Cfg.Dev.Enabled {
			panic("Dev mode is not supported in test mode")
		}
	}

	if Cfg.Db.Enabled {
		initDB()
	}

	if Cfg.TenantProvider == "configmap" {
		GetLabelsFunc = GetLabelsCM
	}
	if Cfg.TenantProvider == "mysql" {
		GetLabelsFunc = GetLabelsDB
	}
	if GetLabelsFunc == nil {
		Logger.Panic("Tenant provider not supported")
	}

	Logger.Info("------Init Complete------")

}

// initConfig initializes the configuration from the files `config` and `labels` using Viper.
func initConfig() {
	Cfg = &Config{}
	V = viper.NewWithOptions(viper.KeyDelimiter("::"))
	loadConfig("config")
	if Cfg.TenantProvider == "configmap" {
		loadConfig("labels")
	}
}

// onConfigChange is a callback that gets triggered when a configuration file changes.
// It reloads the configuration from the files `config` and `labels`.
func onConfigChange(e fsnotify.Event) {
	//Todo: change log level on reload
	Cfg = &Config{}
	var configs []string
	if Cfg.TenantProvider == "configmap" {
		configs = []string{"config", "labels"}
	} else {
		configs = []string{"config"}
	}

	for _, name := range configs {
		V.SetConfigName(name) // name of config file (without extension)
		err := V.MergeInConfig()
		if err != nil { // Handle errors reading the config file
			Logger.Panic("Error while reading config file", zap.Error(err))
		}
		err = V.Unmarshal(Cfg)
		if err != nil { // Handle errors reading the config file
			Logger.Panic("Error while unmarshalling config file", zap.Error(err))
		}
	}
	Logger.Info("Config reloaded", zap.Any("config", Cfg))
	Logger.Info("Config file changed", zap.String("file", e.Name))
	updateLogLevel()
	initTLSConfig()
	initJWKS()
}

// loadConfig loads the configuration from the specified file. It looks for the config file
// in the `/etc/config/` directory and the `./configs` directory.
func loadConfig(configName string) {
	V.SetConfigName(configName) // name of config file (without extension)
	V.SetConfigType("yaml")
	Logger.Info("Looking for config in /etc/config/", zap.String("configName", configName))
	V.AddConfigPath(fmt.Sprintf("/etc/config/%s/", configName))
	V.AddConfigPath("./configs")
	err := V.MergeInConfig() // Find and read the config file
	if err != nil {          // Handle errors reading the config file
		Logger.Panic("Error while reading config file", zap.Error(err))
	}
	err = V.Unmarshal(Cfg)
	if err != nil { // Handle errors reading the config file
		Logger.Panic("Error while unmarshalling config file", zap.Error(err))
	}
	V.OnConfigChange(onConfigChange)
	V.WatchConfig()
}

// initLogging initializes the logger based on the log level specified in the config file.
func initLogging() *zap.Logger {
	atomicLevel = zap.NewAtomicLevel()
	atomicLevel.SetLevel(getZapLevel("info"))

	rawJSON := []byte(`{
		"level": "info",
		"encoding": "json",
		"outputPaths": ["stdout"],
		"errorOutputPaths": ["stdout"],
		"encoderConfig": {
		  "messageKey": "msg",
		  "levelKey": "level",
		  "levelEncoder": "lowercase"
		}
	  }`)

	var cfg zap.Config
	if err := json.Unmarshal(rawJSON, &cfg); err != nil {
		panic(err)
	}
	cfg.Level = atomicLevel
	Logger = zap.Must(cfg.Build())

	Logger.Debug("logger construction succeeded")
	Logger.Debug("Go Version", zap.String("version", runtime.Version()))
	Logger.Debug("Go OS/Arch", zap.String("os", runtime.GOOS), zap.String("arch", runtime.GOARCH))
	Logger.Debug("Config", zap.Any("cfg", Cfg))
	return Logger
}

func getZapLevel(level string) zapcore.Level {
	switch level {
	case "debug":
		return zapcore.DebugLevel
	case "info":
		return zapcore.InfoLevel
	case "warn":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	case "fatal":
		return zapcore.FatalLevel
	default: // unknown level or not set, default to info
		return zapcore.InfoLevel
	}
}

func updateLogLevel() {
	atomicLevel.SetLevel(getZapLevel(strings.ToLower(Cfg.Log.Level)))
}

func initTLSConfig() {
	rootCAs, _ := x509.SystemCertPool()
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}

	if Cfg.Web.TrustedRootCaPath != "" {
		err := filepath.Walk(Cfg.Web.TrustedRootCaPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || strings.Contains(info.Name(), "..") {
				return nil
			}

			certs, err := os.ReadFile(path)
			if err != nil {
				Logger.Error("Error while reading trusted CA", zap.Error(err))
				return err
			}
			Logger.Debug("Adding trusted CA", zap.String("path", path))
			certs = append(certs, []byte("\n")...)
			rootCAs.AppendCertsFromPEM(certs)

			return nil
		})

		if err != nil {
			Logger.Error("Error while traversing directory", zap.Error(err))
		}
	}

	var certificates []tls.Certificate

	lokiCert, err := tls.LoadX509KeyPair(Cfg.Loki.Cert, Cfg.Loki.Key)
	if err != nil {
		Logger.Error("Error while loading loki certificate", zap.Error(err))
	} else {
		Logger.Debug("Adding Loki certificate", zap.String("path", Cfg.Loki.Cert))
		certificates = append(certificates, lokiCert)
	}

	thanosCert, err := tls.LoadX509KeyPair(Cfg.Thanos.Cert, Cfg.Thanos.Key)
	if err != nil {
		Logger.Error("Error while loading thanos certificate", zap.Error(err))
	} else {
		Logger.Debug("Adding Thanos certificate", zap.String("path", Cfg.Loki.Cert))
		certificates = append(certificates, thanosCert)
	}

	config := &tls.Config{
		InsecureSkipVerify: Cfg.Web.InsecureSkipVerify,
		RootCAs:            rootCAs,
		Certificates:       certificates,
	}

	http.DefaultTransport.(*http.Transport).TLSClientConfig = config
}

// initJWKS initializes the JWKS (JSON Web Key Set) from a specified URL. It sets up the refresh parameters
// for the JWKS and handles any errors that occur during the refresh.
func initJWKS() {
	Logger.Info("Init Keycloak config")
	jwksURL := Cfg.Web.JwksCertURL

	options := keyfunc.Options{
		RefreshErrorHandler: func(err error) {
			if err != nil {
				Logger.Error("Error serving Keyfunc", zap.Error(err))
			}
		},
		RefreshInterval:   time.Hour,
		RefreshRateLimit:  time.Minute * 5,
		RefreshTimeout:    time.Second * 10,
		RefreshUnknownKID: true,
	}

	// Create the JWKS from the resource at the given URL.
	err := error(nil)
	Jwks, err = keyfunc.Get(jwksURL, options)
	if err != nil {
		Logger.Panic("Error init jwks", zap.Error(err))
	}
	Logger.Info("Finished Keycloak config")
}

// initDB establishes a connection to the database if the `Db.Enabled` configuration setting is `true`.
// It reads the database password from a file, sets up the database connection configuration,
// and opens the database connection.
func initDB() {
	password, err := os.ReadFile(Cfg.Db.PasswordPath)
	if err != nil {
		Logger.Panic("Could not read db password", zap.Error(err))
	}
	cfg := mysql.Config{
		User:                 Cfg.Db.User,
		Passwd:               string(password),
		Net:                  "tcp",
		AllowNativePasswords: true,
		Addr:                 fmt.Sprintf("%s:%d", Cfg.Db.Host, Cfg.Db.Port),
		DBName:               Cfg.Db.DbName,
	}
	// Get a database handle.
	DB, err = sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		Logger.Panic("Error opening DB connection", zap.Error(err))

	}

}
