package main

import (
	"context"
	"github.com/TicketsBot/archiverclient"
	"github.com/TicketsBot/cleanupdaemon/config"
	"github.com/TicketsBot/cleanupdaemon/daemon"
	"github.com/TicketsBot/database"
	"github.com/jackc/pgx/v4/pgxpool"
	"time"
)

func main() {
	conf := config.Parse()

	pool, err := pgxpool.Connect(context.Background(), conf.DatabaseUri)
	if err != nil {
		panic(err)
	}

	db := database.NewDatabase(pool)

	// encryption key is not used
	client := archiverclient.NewArchiverClient(conf.LogArchiverUri, nil)

	daemon := daemon.NewDaemon(&client, db)
	daemon.Run()

	if !conf.OneShot {
		for {
			time.Sleep(time.Hour * 6)
			daemon.Run()
		}
	}
}
