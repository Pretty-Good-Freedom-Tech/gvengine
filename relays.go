package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"gorm.io/gorm"
)

var nostrSubs []*nostr.Subscription
var nostrRelays []*nostr.Relay

func isHex(s string) bool {
	dst := make([]byte, hex.DecodedLen(len(s)))

	if _, err := hex.Decode(dst, []byte(s)); err != nil {
		return false
		// s is not a valid
	}
	return true
}

func watchInterrupt() {
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		TheLog.Println("exiting gracefully")
		for _, s := range nostrSubs {
			s.Unsub()
			s.Close()
		}
		for _, r := range nostrRelays {
			TheLog.Printf("Closing connection to relay: %s\n", r.URL)
			r.Close()
			UpdateOrCreateRelayStatus(DB, r.URL, "connection error: app exit", "")
		}
		// give other relays time to close
		time.Sleep(3 * time.Second)
		os.Exit(0)
	}()
}

func doRelay(db *gorm.DB, ctx context.Context, url string, pubkey string) bool {
	// check if connection already established
	var fr RelayStatus
	db.Model(fr).Where("url = ? and metadata_pubkey = ?", url, pubkey).First(&fr)
	if strings.Contains(fr.Status, "established") {
		return true
	}

	relay, err := nostr.RelayConnect(ctx, url)
	if err != nil {
		TheLog.Printf("failed initial connection to relay: %s, %s; skipping relay", url, err)
		UpdateOrCreateRelayStatus(db, url, "failed initial connection", pubkey)
		return false
	}
	nostrRelays = append(nostrRelays, relay)

	UpdateOrCreateRelayStatus(db, url, "connection established", pubkey)

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
	db.Where("url = ? and metadata_pubkey = ?", url, pubkey).First(&rs)

	sinceDisco := rs.LastDisco
	if sinceDisco.IsZero() {
		sinceDisco = time.Now().Add(-72 * time.Hour)
		TheLog.Printf("no known last disco time for %s, defaulting to 72 hrs\n", url)
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
	} else {
		var authorPubkeys []string
		for _, a := range thisHopFollows {
			authorPubkeys = append(authorPubkeys, a.PubkeyHex)
		}
		hop2Filters = append(hop2Filters, nostr.Filter{
			Kinds:   []int{3, 0},
			Limit:   1000,
			Authors: authorPubkeys,
			Since:   &filterTimestamp,
		})
	}

	hop2Sub, _ := relay.Subscribe(ctx, hop2Filters)
	nostrSubs = append(nostrSubs, hop2Sub)

	go func() {
		processSub(sub, relay, pubkey)
	}()

	go func() {
		processSub(hop2Sub, relay, pubkey)
	}()

	return true
}

func processSub(sub *nostr.Subscription, relay *nostr.Relay, pubkey string) {

	go func() {
		<-sub.EndOfStoredEvents
		TheLog.Printf("got EOSE from %s\n", relay.URL)
		UpdateOrCreateRelayStatus(DB, relay.URL, "connection established: EOSE", pubkey)
	}()

	if sub != nil {
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

}
