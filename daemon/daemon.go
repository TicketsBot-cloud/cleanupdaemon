package daemon

import (
	"github.com/TicketsBot/archiverclient"
	"github.com/TicketsBot/database"
	"log"
	"os"
	"time"
)

const BreakTime = time.Second

type Daemon struct {
	logger   *log.Logger
	client   *archiverclient.ArchiverClient
	database *database.Database
}

func NewDaemon(client *archiverclient.ArchiverClient, database *database.Database) *Daemon {
	return &Daemon{
		logger:   log.New(os.Stderr, "[daemon] ", log.LstdFlags),
		client:   client,
		database: database,
	}
}

func (d *Daemon) Run() {
	guildIds, err := d.database.GuildLeaveTime.GetBefore(time.Hour * 24 * 30)
	if err != nil {
		log.Printf("error occurred while fetching guild ids: %s\n", err.Error())
		return
	}

	var success []uint64
	for _, guildId := range guildIds {
		if err := d.client.PurgeGuild(guildId); err != nil {
			log.Printf("error sending purge request: %s\n", err.Error())
			time.Sleep(BreakTime)
			continue
		}

		success = append(success, guildId)
		time.Sleep(BreakTime)
	}

	if err := d.database.GuildLeaveTime.DeleteAll(success); err != nil {
		d.logger.Printf("error while deleting leave times: %s\n", err.Error())
	}
}
