package config

import "github.com/caarlos0/env/v6"

type Config struct {
	DatabaseUri     string `env:"DB_URI"`
	LogArchiverUri  string `env:"LOG_ARCHIVER_URI"`
	OneShot         bool   `env:"ONESHOT"`
	ProductionMode  bool   `env:"PRODUCTION_MODE" envDefault:"false"`
	SentryDsn       string `env:"SENTRY_DSN"`
	MainBotToken    string `env:"MAIN_BOT_TOKEN"`
	DiscordProxyUrl string `env:"DISCORD_PROXY_URL"`
}

func Parse() (conf Config) {
	if err := env.Parse(&conf); err != nil {
		panic(err)
	}

	return
}
