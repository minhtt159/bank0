// Package config loads application configuration (config.yaml + .env + env vars).
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/spf13/viper"
)

type Config struct {
	App      AppConfig      `mapstructure:"app"`
	Logging  LoggingConfig  `mapstructure:"logging"`
	Database DatabaseConfig `mapstructure:"database"`
	Server   ServerConfig   `mapstructure:"server"`
	Admin    AdminConfig    `mapstructure:"admin"`
	Auth     AuthConfig     `mapstructure:"auth"`
}

// AuthConfig configures JWT bearer auth for the client API surface (the portal
// uses DB-backed cookie sessions instead). The secret MUST be set and shared in
// production / multi-replica; an empty secret falls back to an insecure dev value
// with a loud warning.
type AuthConfig struct {
	JWTSecret          string        `mapstructure:"jwt_secret"`
	JWTTTL             time.Duration `mapstructure:"jwt_ttl"`
	JWTIssuer          string        `mapstructure:"jwt_issuer"`
	JWTAudience        string        `mapstructure:"jwt_audience"`
	RefreshTTL         time.Duration `mapstructure:"refresh_ttl"`          // idle window, slid on rotate
	RefreshAbsoluteTTL time.Duration `mapstructure:"refresh_absolute_ttl"` // hard cap per family
}

type AppConfig struct {
	Name    string `mapstructure:"name"`
	Version string `mapstructure:"version"`
	Env     string `mapstructure:"env"`
	Debug   bool   `mapstructure:"debug"`
}

type LoggingConfig struct {
	Level    string `mapstructure:"level"`    // debug|info|warn|error
	Encoding string `mapstructure:"encoding"` // json|console
}

type DatabaseConfig struct {
	DSN          string        `mapstructure:"dsn"`
	MaxOpenConns int           `mapstructure:"max_open_conns"`
	MaxIdleConns int           `mapstructure:"max_idle_conns"`
	ConnTimeout  time.Duration `mapstructure:"conn_timeout"`
}

type ServerConfig struct {
	Port             int    `mapstructure:"port"`
	DefaultPageLimit int32  `mapstructure:"default_page_limit"`
	// Mode selects which route surface this instance serves:
	//   "api"    -> client API only      (api.bank0.hnimn.art)
	//   "portal" -> admin API + console   (portal.bank0.hnimn.art)
	//   "all"    -> everything            (local docker-compose, single container)
	Mode            string `mapstructure:"mode"`
	OpenAPISpecPath string `mapstructure:"openapi_spec_path"`
	// AutoMigrate runs embedded migrations on startup. Handy for local
	// docker-compose (1 replica); leave false in K8s and use the migrate Job.
	AutoMigrate bool `mapstructure:"auto_migrate"`
}

// AdminConfig holds operator-console policy knobs. MakerCheckerThresholdMinor is
// the configurable 4-eyes limit: money moves above it require a second approver
// (enforced in the API/console layer, not the DB).
type AdminConfig struct {
	MakerCheckerThresholdMinor int64         `mapstructure:"maker_checker_threshold_minor"`
	SessionIdleTimeout         time.Duration `mapstructure:"session_idle_timeout"`
	MaintenanceInterval        time.Duration `mapstructure:"maintenance_interval"` // expire_holds / cleanup cadence
	// RunMaintenance enables the in-process maintenance loop on this instance.
	// Advisory-locked, so it's safe everywhere; in K8s we enable it only on the
	// portal deployment to avoid needless ticking across many api replicas.
	RunMaintenance bool `mapstructure:"run_maintenance"`
}

func LoadConfig(path string) (Config, error) {
	_ = godotenv.Load()

	v := viper.New()

	v.SetDefault("app.name", "bank0")
	v.SetDefault("app.version", "0.1.0")
	v.SetDefault("app.env", "development")
	v.SetDefault("app.debug", true)

	v.SetDefault("logging.level", "debug")
	v.SetDefault("logging.encoding", "console")

	v.SetDefault("database.max_open_conns", 10)
	v.SetDefault("database.max_idle_conns", 5)
	v.SetDefault("database.conn_timeout", "5s")

	v.SetDefault("server.port", 8080)
	v.SetDefault("server.default_page_limit", 25)
	v.SetDefault("server.mode", "all")
	v.SetDefault("server.openapi_spec_path", "api/openapi.yaml")
	v.SetDefault("server.auto_migrate", false)

	v.SetDefault("admin.maker_checker_threshold_minor", 1000000) // €10,000.00
	v.SetDefault("admin.session_idle_timeout", "30m")
	v.SetDefault("admin.maintenance_interval", "60s")
	v.SetDefault("admin.run_maintenance", true)

	v.SetDefault("auth.jwt_secret", "")
	v.SetDefault("auth.jwt_ttl", "15m") // short access token; clients rotate via /auth/refresh
	v.SetDefault("auth.jwt_issuer", "bank0")
	v.SetDefault("auth.jwt_audience", "bank0-client")
	v.SetDefault("auth.refresh_ttl", "720h")          // 30d idle
	v.SetDefault("auth.refresh_absolute_ttl", "2160h") // 90d hard cap

	v.AddConfigPath(path)
	v.SetConfigName("config")
	v.SetConfigType("yaml")

	v.SetEnvPrefix("APP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}
	return cfg, nil
}
