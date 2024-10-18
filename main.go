package main

import (
	"context"
	"fmt"
	"os"
)

var AppInfo = "gvengine v0.0.1"

func main() {

	/*
		err := godotenv.Load()
		if err != nil {
			log.Info("Error loading .env file, using defaults")
		}
	*/

	DB = GetGormConnection()

	migrateErr := DB.AutoMigrate(&Metadata{})
	migrateErr1 := DB.AutoMigrate(&RelayStatus{})
	migrateErr2 := DB.AutoMigrate(&WotScore{})
	migrateErr3 := DB.AutoMigrate(&GvScore{})

	migrateErrs := []error{
		migrateErr,
		migrateErr1,
		migrateErr2,
		migrateErr3,
	}

	for i, err := range migrateErrs {
		if err != nil {
			fmt.Println("Error running a migration (%d) %s\nexiting.", i, err)
			os.Exit(1)
		}
	}

	CTX := context.Background()
	relayUrls := []string{
		"wss://relay.damus.io",
		"wss://profiles.nostr1.com",
		"wss://nostr21.com",
		"wss://relay.primal.net",
		"wss://purplepag.es",
		"wss://nos.lol",
	}

	var p Metadata

	// todo, select various members and loop over em etc..
	DB.Where("member = ?", true).First(&p)

	for _, url := range relayUrls {
		doRelay(DB, CTX, url, p.PubkeyHex)
	}

	calculateWot(p.PubkeyHex)

	select {}
}
