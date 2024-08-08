package main

import (
	"context"
	"fmt"
	"github.com/TicketsBot/archiverclient"
	"github.com/TicketsBot/cleanupdaemon/pkg/config"
	"github.com/TicketsBot/cleanupdaemon/pkg/daemon"
	"github.com/TicketsBot/common/observability"
	"github.com/TicketsBot/database"
	"github.com/getsentry/sentry-go"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/rxdn/gdl/rest/request"
	"go.uber.org/zap"
	"net/http"
	"time"
)

func main() {
	conf := config.Parse()

	if err := sentry.Init(sentry.ClientOptions{
		Dsn:   conf.SentryDsn,
		Debug: !conf.ProductionMode,
	}); err != nil {
		if conf.ProductionMode {
			panic(err)
		} else {
			fmt.Printf("Failed to initialise sentry: %v\n", err)
		}
	}

	var logger *zap.Logger
	var err error
	if conf.ProductionMode {
		logger, err = zap.NewProduction(
			zap.AddCaller(),
			zap.AddStacktrace(zap.ErrorLevel),
			zap.WrapCore(observability.ZapSentryAdapter(observability.EnvironmentProduction)),
		)
	} else {
		logger, err = zap.NewDevelopment(zap.AddCaller(), zap.AddStacktrace(zap.ErrorLevel))
	}

	if err != nil {
		panic(err)
	}

	logger.Debug("Connecting to database...")
	pool, err := pgxpool.Connect(context.Background(), conf.DatabaseUri)
	if err != nil {
		panic(err)
	}

	logger.Debug("Connected to database")

	db := database.NewDatabase(pool)

	logger.Debug("Built database client")

	// encryption key is not used
	client := archiverclient.NewArchiverClient(conf.LogArchiverUri, nil)

	logger.Debug("Built archiver client")

	request.RegisterPreRequestHook(func(token string, req *http.Request) {
		if len(conf.DiscordProxyUrl) > 0 {
			req.URL.Scheme = "http"
			req.URL.Host = conf.DiscordProxyUrl
		}
	})

	daemon := daemon.NewDaemon(logger, conf, &client, db)
	daemon.Run()

	if !conf.OneShot {
		for {
			time.Sleep(time.Hour * 6)
			daemon.Run()
		}
	}
}
