package main

import (
	"context"
	"fmt"
	"os"
)

var AppInfo = "gvengine v0.0.1"

// THE NOSTR PUBKEY TO BASE CALCULATIONS OFF:
var pubkey = "7cc328a08ddb2afdf9f9be77beff4c83489ff979721827d628a542f32a247c0e"

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

	migrateErrs := []error{
		migrateErr,
		migrateErr1,
		migrateErr2,
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

	for _, url := range relayUrls {
		doRelay(DB, CTX, url)
	}

	calculateWot(pubkey)

	select {}
}
