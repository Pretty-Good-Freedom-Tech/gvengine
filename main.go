package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/mux"
)

var AppInfo = "gvengine v0.0.1"

var CTX = context.Background()
var relayUrls = []string{
	"wss://relay.damus.io",
	"wss://profiles.nostr1.com",
	"wss://nostr21.com",
	"wss://relay.primal.net",
	//"wss://purplepag.es",
	"wss://nos.lol",
	"wss://wot.utxo.one",
}

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

	r := mux.NewRouter()
	r.HandleFunc("/", HomeHandler)
	r.HandleFunc("/api/members/{key}/gvscores/{pubkey}", GVScoresHandlerPubkey)
	r.HandleFunc("/api/members/{key}/wotscores/{pubkey}", WotScoresHandlerPubkey)
	r.HandleFunc("/api/members/{key}/gvscores", GVScoresHandler)
	r.HandleFunc("/api/members/{key}/wotscores", WotScoresHandler)
	r.HandleFunc("/api/members/{key}/calculate", CalculateScoresHandler)
	r.HandleFunc("/api/members/{key}/scrape", ScrapeRelaysHandler)
	r.HandleFunc("/api/members/{key}/follows", FollowsHandler)
	r.HandleFunc("/api/members/{key}/followers", FollowersHandler)
	http.Handle("/", r)

	srv := &http.Server{
		Addr: "127.0.0.1:8080",
		// Good practice to set timeouts to avoid Slowloris attacks.
		WriteTimeout: time.Second * 15,
		ReadTimeout:  time.Second * 15,
		IdleTimeout:  time.Second * 60,
		Handler:      r, // Pass our instance of gorilla/mux in.
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil {
			fmt.Println(err)
		}
	}()

	go watchInterrupt()

	select {}
}

func HomeHandler(w http.ResponseWriter, r *http.Request) {
	//vars := mux.Vars(r)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func GVScoresHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	vars := mux.Vars(r)
	var scores []GvScore
	DB.Where("metadata_pubkey = ?", vars["key"]).Find(&scores)
	json.NewEncoder(w).Encode(scores)
}

func GVScoresHandlerPubkey(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	vars := mux.Vars(r)
	var scores GvScore
	TheLog.Println(vars)
	DB.Model(&scores).Where("pubkey_hex = ? and metadata_pubkey = ?", vars["pubkey"], vars["key"]).First(&scores)
	json.NewEncoder(w).Encode(scores)
}

func WotScoresHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	vars := mux.Vars(r)
	var scores []WotScore
	DB.Where("metadata_pubkey = ?", vars["key"]).Find(&scores)
	json.NewEncoder(w).Encode(scores)
}

func WotScoresHandlerPubkey(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	vars := mux.Vars(r)
	var scores []WotScore
	DB.Model(&scores).Where("pubkey_hex = ? and metadata_pubkey = ?", vars["pubkey"], vars["key"]).First(&scores)
	json.NewEncoder(w).Encode(scores)
}

func CalculateScoresHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	vars := mux.Vars(r)
	go calculateWot(vars["key"])
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func ScrapeRelaysHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	vars := mux.Vars(r)
	go func() {
		for _, url := range relayUrls {
			doRelay(DB, CTX, url, vars["key"])
		}
	}()
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func FollowersHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	vars := mux.Vars(r)
	var f []string
	DB.Table("metadata_follows").Select("metadata_pubkey_hex").Where("follow_pubkey_hex = ?", vars["key"]).Scan(&f)
	json.NewEncoder(w).Encode(f)
}

func FollowsHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	vars := mux.Vars(r)
	var f []string
	DB.Table("metadata_follows").Select("follow_pubkey_hex").Where("metadata_pubkey_hex = ?", vars["key"]).Scan(&f)
	json.NewEncoder(w).Encode(f)
}
