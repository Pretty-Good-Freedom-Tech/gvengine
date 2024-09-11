package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	//"gorm.io/driver/sqlite"

	"github.com/google/uuid"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var AppInfo = "gvengine v0.0.1"

type Metadata struct {
	PubkeyHex    string `gorm:"primaryKey;size:65"`
	PubkeyNpub   string `gorm:"size:65"`
	Name         string `gorm:"size:1024"`
	About        string `gorm:"size:4096"`
	Nip05        string `gorm:"size:512"`
	Lud06        string `gorm:"size:2048"`
	Lud16        string `gorm:"size:512"`
	Website      string `gorm:"size:512"`
	DisplayName  string `gorm:"size:512"`
	Picture      string `gorm:"type:text;size:65535"`
	TotalFollows int
	UpdatedAt    time.Time `gorm:"autoUpdateTime"`
	// change these defaults to something closer to zero
	ContactsUpdatedAt time.Time   `gorm:"default:current_timestamp(3)"`
	MetadataUpdatedAt time.Time   `gorm:"default:current_timestamp(3)"`
	Follows           []*Metadata `gorm:"many2many:metadata_follows"`
	RawJsonContent    string      `gorm:"type:longtext;size:512000"`
	WotScores         []WotScore  `gorm:"foreignKey:MetadataPubkey;references:PubkeyHex"`
}

type WotScore struct {
	gorm.Model
	ID             uuid.UUID `gorm:"type:char(36);primary_key"`
	MetadataPubkey string    `gorm:"size:65"`
	PubkeyHex      string    `gorm:"size:65"`
	Score          int
}

func (m *WotScore) BeforeCreate(tx *gorm.DB) error {
	m.ID = uuid.New()
	return nil
}

type RelayStatus struct {
	Url       string    `gorm:"primaryKey;size:512"`
	Status    string    `gorm:"size:512"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"`
	// change these defaults to something closer to zero
	LastEOSE  time.Time `gorm:"default:current_timestamp(3)"`
	LastDisco time.Time `gorm:"default:current_timestamp(3)"`
}

var TheLog *log.Logger
var DB *gorm.DB

// THE NOSTR PUBKEY TO BASE CALCULATIONS OFF:
// var pubkey = "HEXPUBKEY"
var nostrSubs []*nostr.Subscription
var nostrRelays []*nostr.Relay

func GetGormConnection() *gorm.DB {
	file, err := os.OpenFile("gv.log", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		// Handle error
		panic(err)
	}

	TheLog = log.New(file, "", log.LstdFlags) // io writer
	newLogger := logger.New(
		TheLog,
		logger.Config{
			SlowThreshold:             time.Second,  // Slow SQL threshold
			LogLevel:                  logger.Error, // Log level
			IgnoreRecordNotFoundError: true,         // Ignore ErrRecordNotFound error for logger
			Colorful:                  false,        // Disable color
		},
	)

	dsn, foundDsn := os.LookupEnv("DB")
	if !foundDsn {
		//	dsn = "flightless.db?cache=shared&mode=rwc"
		dsn = "flightless:flightless@tcp(127.0.0.1:3307)/gvengine?charset=utf8mb4&parseTime=True&loc=Local"
	}

	db, dberr := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: newLogger})
	//db, dberr := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: newLogger})
	if dberr != nil {
		panic(dberr)
	}
	db.Logger.LogMode(logger.Silent)
	//sql, _ := db.DB()
	//sql.SetMaxOpenConns(1)

	return db
}

func UpdateOrCreateRelayStatus(db *gorm.DB, url string, status string) {
	var r RelayStatus
	if status == "EOSE" {
		r = RelayStatus{Url: url, Status: status, LastEOSE: time.Now()}
	} else if strings.HasPrefix(status, "connection error") {
		r = RelayStatus{Url: url, Status: status, LastDisco: time.Now()}
	} else {
		r = RelayStatus{Url: url, Status: status}
	}
	rowsUpdated := db.Model(&r).Where("url = ?", url).Updates(&r).RowsAffected
	if rowsUpdated == 0 {
		db.Create(&r)
	}
}

func doRelay(db *gorm.DB, ctx context.Context, url string) bool {
	relay, err := nostr.RelayConnect(ctx, url)
	if err != nil {
		TheLog.Printf("failed initial connection to relay: %s, %s; skipping relay", url, err)
		UpdateOrCreateRelayStatus(db, url, "failed initial connection")
		return false
	}
	nostrRelays = append(nostrRelays, relay)

	UpdateOrCreateRelayStatus(db, url, "connection established")

	// what do we need for this pubkey for WoT:

	// the follow list (hop1)
	// the follow list of each follow (hop2)
	// hop3?

	hop1Filters := []nostr.Filter{
		{
			Kinds:   []int{0},
			Limit:   1,
			Authors: []string{pubkey},
		},
		{
			Kinds:   []int{3},
			Limit:   1,
			Authors: []string{pubkey},
		},
	}

	// create a subscription and submit to relay
	sub, _ := relay.Subscribe(ctx, hop1Filters)
	nostrSubs = append(nostrSubs, sub)

	// subscribe to follows for each follow
	person := Metadata{
		PubkeyHex: pubkey,
	}

	var thisHopFollows []Metadata
	db.Model(&person).Association("Follows").Find(&thisHopFollows)

	// Pick up where we left off for this relay based on last EOSE timestamp
	var rs RelayStatus
	db.First(&rs, "url = ?", url)
	sinceDisco := rs.LastDisco
	if sinceDisco.IsZero() {
		sinceDisco = time.Now().Add(-72 * time.Hour)
	}
	since := rs.LastEOSE
	if since.IsZero() {
		since = time.Now().Add(-73 * time.Hour)
	}
	if sinceDisco.After(since) {
		since = sinceDisco
	}

	filterTimestamp := nostr.Timestamp(since.Unix())

	// BATCH filters into chunks of 1000 per filter.
	var hop2Filters []nostr.Filter
	counter := 0
	lastCount := 0
	if len(thisHopFollows) > 1000 {
		for i := range thisHopFollows {
			if i > 0 && i%1000 == 0 {
				begin := i - 1000
				end := counter
				authors := thisHopFollows[begin:end]
				var authorPubkeys []string
				for _, a := range authors {
					authorPubkeys = append(authorPubkeys, a.PubkeyHex)
				}

				hop2Filters = append(hop2Filters, nostr.Filter{
					Kinds:   []int{3, 0},
					Limit:   1000,
					Authors: authorPubkeys,
					Since:   &filterTimestamp,
				})
				TheLog.Printf("adding chunk subscription for %d:%d", begin, end)
				lastCount = counter
			}
			counter += 1
		}
		// leftover
		if lastCount != counter+1 {
			begin := lastCount + 1
			end := len(thisHopFollows) - 1
			remainingAuthors := thisHopFollows[begin:end]
			var authorPubkeys []string
			for _, a := range remainingAuthors {
				authorPubkeys = append(authorPubkeys, a.PubkeyHex)
			}

			TheLog.Printf("adding leftover chunk subscription for %d:%d", lastCount, end)

			hop2Filters = append(hop2Filters, nostr.Filter{
				Kinds:   []int{3, 0},
				Limit:   1000,
				Authors: authorPubkeys,
				Since:   &filterTimestamp,
			})
		}
	}

	hop2Sub, _ := relay.Subscribe(ctx, hop2Filters)
	nostrSubs = append(nostrSubs, hop2Sub)

	go func() {
		processSub(sub, relay)
	}()

	go func() {
		processSub(hop2Sub, relay)
	}()

	return true
}

func processSub(sub *nostr.Subscription, relay *nostr.Relay) {

	go func() {
		<-sub.EndOfStoredEvents
		TheLog.Printf("got EOSE from %s\n", relay.URL)
		UpdateOrCreateRelayStatus(DB, relay.URL, "EOSE")
	}()

	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		TheLog.Println("exiting gracefully")
		sub.Unsub()
		relay.Close()

		UpdateOrCreateRelayStatus(DB, relay.URL, "connection error: app exit")

		// give other relays time to close
		time.Sleep(3 * time.Second)
		os.Exit(0)
	}()

	for ev := range sub.Events {
		TheLog.Printf("got event kind %d from relay %s", ev.Kind, relay.URL)
		if ev.Kind == 0 {
			// Metadata
			m := Metadata{}
			err := json.Unmarshal([]byte(ev.Content), &m)
			unmarshalSuccess := false
			if err != nil {
				TheLog.Printf("%s: %v", err, ev.Content)
				m.RawJsonContent = ev.Content
			} else {
				unmarshalSuccess = true
			}
			m.PubkeyHex = ev.PubKey
			npub, errEncode := nip19.EncodePublicKey(ev.PubKey)
			if errEncode == nil {
				m.PubkeyNpub = npub
			}
			m.MetadataUpdatedAt = ev.CreatedAt.Time()
			m.ContactsUpdatedAt = time.Unix(0, 0)
			if len(m.Picture) > 65535 {
				//TheLog.Println("too big a picture for profile, skipping" + ev.PubKey)
				m.Picture = ""
				//continue
			}
			// check timestamps
			var checkMeta Metadata
			notFoundErr := DB.First(&checkMeta, "pubkey_hex = ?", m.PubkeyHex).Error
			if notFoundErr != nil {
				err := DB.Save(&m).Error
				if err != nil {
					TheLog.Printf("Error saving metadata was: %s", err)
				}
				TheLog.Printf("Created metadata for %s, %s\n", m.Name, m.Nip05)
			} else {
				if checkMeta.MetadataUpdatedAt.After(ev.CreatedAt.Time()) || checkMeta.MetadataUpdatedAt.Equal(ev.CreatedAt.Time()) {
					TheLog.Println("skipping old metadata for " + ev.PubKey)
					continue
				} else {
					rowsUpdated := DB.Model(Metadata{}).Where("pubkey_hex = ?", m.PubkeyHex).Updates(&m).RowsAffected
					if rowsUpdated > 0 {
						TheLog.Printf("Updated metadata for %s, %s\n", m.Name, m.Nip05)
					} else {
						//
						// here we need go store the record anyway, with a pubkey, and the 'rawjson'
						TheLog.Printf("UNCOOL NESTED JSON FOR METADATA DETECTED, falling back to RAW json %v, unmarshalsuccess was: %v", m, unmarshalSuccess)
					}
				}
			}
		} else if ev.Kind == 3 {

			// Contact List
			pTags := []string{"p"}
			allPTags := ev.Tags.GetAll(pTags)
			var person Metadata
			notFoundError := DB.First(&person, "pubkey_hex = ?", ev.PubKey).Error
			if notFoundError != nil {
				//TheLog.Printf("Creating blank metadata for %s\n", ev.PubKey)
				person = Metadata{
					PubkeyHex:    ev.PubKey,
					TotalFollows: len(allPTags),
					// set time to january 1st 1970
					MetadataUpdatedAt: time.Unix(0, 0),
					ContactsUpdatedAt: ev.CreatedAt.Time(),
				}
				DB.Create(&person)
			} else {
				if person.ContactsUpdatedAt.After(ev.CreatedAt.Time()) {
					// double check the timestamp for this follow list, don't update if older than most recent
					TheLog.Printf("skipping old contact list for " + ev.PubKey)
					continue
				} else {
					DB.Model(&person).Omit("updated_at").Update("total_follows", len(allPTags))
					DB.Model(&person).Omit("updated_at").Update("contacts_updated_at", ev.CreatedAt.Time())
					//TheLog.Printf("updating (%d) follows for %s: %s\n", len(allPTags), person.Name, person.PubkeyHex)
				}
			}

			// purge followers that have been 'unfollowed'
			var oldFollows []Metadata
			DB.Model(&person).Association("Follows").Find(&oldFollows)
			for _, oldFollow := range oldFollows {
				found := false
				for _, n := range allPTags {
					if len(n) >= 2 && n[1] == oldFollow.PubkeyHex {
						found = true
					}
				}
				if !found {
					DB.Exec("delete from metadata_follows where metadata_pubkey_hex = ? and follow_pubkey_hex = ?", person.PubkeyHex, oldFollow.PubkeyHex)
				}
			}

			for _, c := range allPTags {
				// if the pubkey fails the sanitization (is a hex value) skip it

				if len(c) < 2 || !isHex(c[1]) {
					TheLog.Printf("skipping invalid pubkey from follow list: %d, %s ", len(c), c[1])
					continue
				}
				var followPerson Metadata
				notFoundFollow := DB.First(&followPerson, "pubkey_hex = ?", c[1]).Error

				if notFoundFollow != nil {
					// follow user not found, need to create it
					var newUser Metadata
					// follow user recommend server suggestion if it exists
					if len(c) >= 3 && c[2] != "" {
						newUser = Metadata{
							PubkeyHex:         c[1],
							ContactsUpdatedAt: time.Unix(0, 0),
							MetadataUpdatedAt: time.Unix(0, 0),
						}
					} else {
						newUser = Metadata{PubkeyHex: c[1], ContactsUpdatedAt: time.Unix(0, 0), MetadataUpdatedAt: time.Unix(0, 0)}
					}
					createNewErr := DB.Omit("Follows").Create(&newUser).Error
					if createNewErr != nil {
						TheLog.Println("Error creating user for follow: ", createNewErr)
					}
					// use gorm insert statement to update the join table
					DB.Exec("insert ignore into metadata_follows (metadata_pubkey_hex, follow_pubkey_hex) values (?, ?)", person.PubkeyHex, newUser.PubkeyHex)
				} else {
					// use gorm insert statement to update the join table
					DB.Exec("insert ignore into metadata_follows (metadata_pubkey_hex, follow_pubkey_hex) values (?, ?)", person.PubkeyHex, followPerson.PubkeyHex)
				}
			}
		}
	}

}

func isHex(s string) bool {
	dst := make([]byte, hex.DecodedLen(len(s)))

	if _, err := hex.Decode(dst, []byte(s)); err != nil {
		return false
		// s is not a valid
	}
	return true
}

func calculateWot(pubkey string) {
	var followersCount int64
	var followsCount int64
	DB.Table("metadata_follows").Where("follow_pubkey_hex = ?", pubkey).Count(&followersCount)
	DB.Table("metadata_follows").Where("metadata_pubkey_hex = ?", pubkey).Count(&followsCount)

	var person Metadata
	DB.FirstOrInit(&person, Metadata{PubkeyHex: pubkey})
	var follows []Metadata
	assocError := DB.Model(&person).Association("Follows").Find(&follows)
	if assocError == nil {
		allHop := make(map[string]Metadata)
		for _, f := range follows {
			fperson := Metadata{PubkeyHex: f.PubkeyHex}
			var hop1follows []Metadata
			assocErrorHop1 := DB.Model(&fperson).Association("Follows").Find(&hop1follows)
			if assocErrorHop1 == nil {
				//TheLog.Printf("WEORE DOING STUFF hop1follows %v", len(hop1follows))
				for _, h1f := range hop1follows {
					allHop[h1f.PubkeyHex] = h1f
				}
			} else {
				//error
			}

		}
		TheLog.Printf("hop1 follows was: %d", len(allHop))

		// Influence score notes::
		// iterate over allHop, and create the scores!
		//dunbarNumber := 100.0
		attenuationFactor := 80.0 / 100.0
		rigor := 25.0 / 100.0
		defaultUserScore := 0.00     // / 100
		defaultUserConfidence := 0.0 // / 100
		followInterpretationScore := 100.0 / 100.0
		followInterpretationConfidence := 5.0 / 100.0

		//			muteInterpretationScore := 0.0 / 100
		//			muteInterpretationConfidence := 10.0 / 100

		infScores := make(map[string]float64)
		avgScores := make(map[string]float64)
		certaintyScores := make(map[string]float64)
		inputScores := make(map[string]float64)

		// initialize scores
		// somethings wrong here, all the initial scores are zero?

		// allHop for influence score * could be every pubkey in the DB.

		for p, _ := range allHop {
			// convert input to certainty
			rigority := -math.Log(rigor)
			fooB := -defaultUserConfidence * rigority
			fooA := math.Exp(fooB)
			TheLog.Printf("fooA was %f", fooA)
			certainty := 1 - fooA
			TheLog.Printf("certainty for %s was %f", p, certainty)
			certaintyScores[p] = certainty
			avgScores[p] = defaultUserScore
			inputScores[p] = defaultUserConfidence
			infScores[p] = certainty * defaultUserScore
			TheLog.Printf("initial score for %s was %f", p, infScores[p])
		}

		// initialize my score
		infScores[pubkey] = 1.0
		avgScores[pubkey] = 1.0
		inputScores[pubkey] = 9999
		certaintyScores[pubkey] = 1.0
		// make sure YOUR score never gets overwritten ^^^

		// add mypubkey to allHops ? nah doesn't matter
		allHop[pubkey] = person

		// cycle scores
		for i := 0; i < 8; i++ {
			for pkRatee, _ := range allHop {
				if pkRatee != pubkey {
					sumOfWeights := 0.0
					sumOfProducts := 0.0
					// unused?
					//sumOfWeightsDirectRatings := 0.0

					var thisHopFollowers []string
					DB.Table("metadata_follows").Select("metadata_pubkey_hex").Where("follow_pubkey_hex = ?", pkRatee).Scan(&thisHopFollowers)
					//TheLog.Printf("Found %d follows for %s", len(thisHopFollowers), pkRatee)

					for _, pkRater := range thisHopFollowers {
						if pkRater != pkRatee {
							rating := float64(followInterpretationScore)
							weight := float64(attenuationFactor) * infScores[pkRater] * float64(followInterpretationConfidence)
							if pkRater == pubkey {
								// no attenuationFactor
								weight = infScores[pkRater] * float64(followInterpretationConfidence)
							}

							product := weight * rating
							sumOfWeights += float64(weight)
							sumOfProducts += float64(product)
						}
					}

					// mutes: todo

					if sumOfWeights > 0 {
						TheLog.Printf("sumofWeights was %f", sumOfWeights)
						average := (sumOfProducts / sumOfWeights)
						input := sumOfWeights

						// convert input to certainty
						rigority := -math.Log(rigor)
						fooB := -input * rigority
						fooA := math.Exp(fooB)
						certainty := 1 - fooA
						influence := average * certainty
						infScores[pkRatee] = float64(influence)
						avgScores[pkRatee] = average
						certaintyScores[pkRatee] = certainty
						inputScores[pkRatee] = input
					}

				}
			}
			TheLog.Printf("calculated influence cycle %d", i)
		}

		for p, s := range infScores {
			if s > 0 {
				TheLog.Printf("Influence score for %s: %f", p, s)
			}
		}
		TheLog.Printf("Calculated %d total influence scores", len(infScores))

		// wot scores
		wotScores := make(map[string]int)

		TheLog.Printf("calculating scores .... please wait \n")
		for pk, _ := range allHop {
			//var thisHopFollows []Metadata
			//DB.Model(&person).Association("Follows").Find(&thisHopFollows)

			var thisHopFollowers []string
			DB.Table("metadata_follows").Select("metadata_pubkey_hex").Where("follow_pubkey_hex = ?", pk).Scan(&thisHopFollowers)

			intersection := make(map[string]bool)
			// intersection
			for _, follower := range thisHopFollowers {
				for _, follow := range follows {
					if follower == follow.PubkeyHex {
						intersection[follower] = true
					}
				}
			}

			wotScores[pk] = len(intersection)
		}

		//DB.Model(&person).Association("WotScores").Clear()

		DB.Unscoped().Model(&person).Association("WotScores").Unscoped().Clear()

		TheLog.Printf("saving scores .... please wait \n")
		for p, s := range wotScores {
			DB.Model(&WotScore{}).Create(&WotScore{
				MetadataPubkey: person.PubkeyHex,
				Score:          s,
				PubkeyHex:      p,
			})
		}

		// deletes all associations
		// FOR REFERENCE HOW NOT TO UPDATE ASSOCIATIONS RESULTS IN:
		// "too many prepared statements" even with batching
		/*
			counter := 0
			lastCount := 0
			if len(scores) > 500 {
				for _ = range scores {
					if counter > 0 && counter%500 == 0 {
						begin := counter - 500
						end := counter
						batch := scores[begin:end]
						DB.Model(&person).Association("WotScores").Append(&batch)
						TheLog.Printf("batching batch: %d, %d", begin, end)
						lastCount = counter
						time.Sleep(time.Second * 1)
					}
					counter += 1
				}
				if lastCount != counter+1 {
					begin := lastCount
					end := len(scores) - 1
					remainingBatch := scores[begin:end]
					DB.Model(&person).Association("WotScores").Append(&remainingBatch)
					TheLog.Printf("remaining batch: %d, %d", begin, end)
				}

			} else {
				DB.Model(&person).Association("WotScores").Append(&scores)
			}
		*/

		// todo: cleanup leftover scores that have left the wot

		//TheLog.Printf("total number scored: %d, %d", len(wotScores))
		TheLog.Printf("pubkey %s, follows: %d, followers: %d", person.PubkeyHex, followsCount, followersCount)
	}
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
