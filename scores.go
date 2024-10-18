package main

import (
	"math"
)

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
			//TheLog.Printf("fooA was %f", fooA)
			certainty := 1 - fooA
			//TheLog.Printf("certainty for %s was %f", p, certainty)
			certaintyScores[p] = certainty
			avgScores[p] = defaultUserScore
			inputScores[p] = defaultUserConfidence
			infScores[p] = certainty * defaultUserScore
			//TheLog.Printf("initial score for %s was %f", p, infScores[p])
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
			TheLog.Printf("calculated influence cycle %d\n", i)
		}

		TheLog.Printf("Calculated %d total influence scores\n", len(infScores))

		DB.Unscoped().Model(&person).Association("GvScores").Unscoped().Clear()
		TheLog.Printf("saving influence scores..")
		for p, s := range infScores {
			if s > 0 {
				DB.Model(&GvScore{}).Create(&GvScore{
					MetadataPubkey: person.PubkeyHex,
					PubkeyHex:      p,
					Score:          s,
				})
				//TheLog.Printf("Influence score for %s: %f", p, s)
			}
		}
		TheLog.Printf("done.\n")

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
