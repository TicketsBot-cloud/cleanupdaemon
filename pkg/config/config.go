package config

import "github.com/caarlos0/env/v6"

type Config struct {
	DatabaseUri            string `env:"DB_URI"`
	CacheUri               string `env:"CACHE_URI"`
	LogArchiverUri         string `env:"LOG_ARCHIVER_URI"`
	OneShot                bool   `env:"ONESHOT"`
	ProductionMode         bool   `env:"PRODUCTION_MODE" envDefault:"false"`
	RetentionPeriodDays    int    `env:"RETENTION_PERIOD_DAYS" envDefault:"30"`
	PurgeConcurrency       int    `env:"PURGE_CONCURRENCY" envDefault:"1"`
	ReconcileIntervalHours int    `env:"RECONCILE_INTERVAL_HOURS" envDefault:"24"`
	ServerCounterUrl       string `env:"SERVER_COUNTER_URL"`
	MinReferenceGuilds     int    `env:"MIN_REFERENCE_GUILDS" envDefault:"100000"`
	SentryDsn              string `env:"SENTRY_DSN"`
	MainBotToken           string `env:"MAIN_BOT_TOKEN"`
	DiscordProxyUrl        string `env:"DISCORD_PROXY_URL"`

	WhitelabelTokenKeys             string `env:"WHITELABEL_TOKEN_KEYS"`
	WhitelabelTokenActiveKeyVersion string `env:"WHITELABEL_TOKEN_ACTIVE_KEY_VERSION"`
}

func Parse() (conf Config) {
	if err := env.Parse(&conf); err != nil {
		panic(err)
	}

	return
}
