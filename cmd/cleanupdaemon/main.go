package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/TicketsBot-cloud/archiverclient"
	"github.com/TicketsBot-cloud/cleanupdaemon/pkg/config"
	"github.com/TicketsBot-cloud/cleanupdaemon/pkg/daemon"
	"github.com/TicketsBot-cloud/common/encryption"
	"github.com/TicketsBot-cloud/common/observability"
	"github.com/TicketsBot-cloud/database"
	"github.com/getsentry/sentry-go"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/rxdn/gdl/rest/request"
	"go.uber.org/zap"
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

	tokenKeyring, err := encryption.ParseKeyring(conf.WhitelabelTokenKeys, conf.WhitelabelTokenActiveKeyVersion)
	if err != nil {
		logger.Fatal("Invalid whitelabel token key configuration", zap.Error(err))
	}

	logger.Debug("Connecting to database...")
	pool, err := pgxpool.Connect(context.Background(), conf.DatabaseUri)
	if err != nil {
		panic(err)
	}

	logger.Debug("Connected to database")

	db := database.NewDatabase(pool)

	var cachePool *pgxpool.Pool
	if conf.CacheUri != "" {
		cachePool, err = pgxpool.Connect(context.Background(), conf.CacheUri)
		if err != nil {
			panic(err)
		}

		logger.Debug("Connected to cache database")
	} else if conf.ProductionMode {
		logger.Error("CACHE_URI is not set, so cache rows of departed guilds will not be purged")
	}

	logger.Debug("Built database client")

	// encryption key is not used
	retriever := archiverclient.NewProxyRetriever(conf.LogArchiverUri)
	client := archiverclient.NewPurgingClient(retriever, nil)

	logger.Debug("Built archiver client")

	request.RegisterPreRequestHook(func(token string, req *http.Request) {
		if len(conf.DiscordProxyUrl) > 0 {
			req.URL.Scheme = "http"
			req.URL.Host = conf.DiscordProxyUrl
		}
	})

	daemon := daemon.NewDaemon(logger, conf, client, db, pool, cachePool, tokenKeyring)

	// sentry-go buffers on an async transport; ONESHOT would otherwise drop the
	// final run's events on return.
	defer sentry.Flush(time.Second * 2)
	defer func() { _ = logger.Sync() }()
	defer pool.Close()
	if cachePool != nil {
		defer cachePool.Close()
	}

	ctx := context.Background()
	reconcileInterval := time.Duration(conf.ReconcileIntervalHours) * time.Hour

	if conf.OneShot {
		if reconcileInterval > 0 {
			if err := daemon.Reconcile(ctx); err != nil {
				logger.Error("Guild reconciliation aborted", zap.Error(err))
			}
		}

		daemon.Run(ctx)
		return
	}

	// Separate goroutine because a Run working through a large backlog can take days.
	if reconcileInterval > 0 {
		go daemon.RunReconciler(ctx, reconcileInterval)
	} else {
		logger.Info("Guild reconciliation disabled")
	}

	for {
		daemon.Run(ctx)
		time.Sleep(time.Hour * 6)
	}
}
