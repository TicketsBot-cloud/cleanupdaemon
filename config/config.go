package config

import "github.com/caarlos0/env/v6"

type Config struct {
	DatabaseUri    string `env:"DB_URI"`
	LogArchiverUri string `env:"LOG_ARCHIVER_URI"`
	OneShot        bool   `env:"ONESHOT"`
}

func Parse() (conf Config) {
	if err := env.Parse(&conf); err != nil {
		panic(err)
	}

	return
}
