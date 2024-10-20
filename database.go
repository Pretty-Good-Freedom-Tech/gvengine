package main

import (
	"log"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

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
	GvScores          []GvScore   `gorm:"foreignKey:MetadataPubkey;references:PubkeyHex"`
	Member            bool        `gorm:"default:false"`
}

type WotScore struct {
	ID             uuid.UUID `gorm:"type:char(36);primary_key"`
	MetadataPubkey string    `gorm:"size:65"`
	PubkeyHex      string    `gorm:"size:65"`
	Score          int
}

type GvScore struct {
	ID             uuid.UUID `gorm:"type:char(36);primary_key"`
	MetadataPubkey string    `gorm:"size:65"`
	PubkeyHex      string    `gorm:"size:65"`
	Score          float64
}

func (m *WotScore) BeforeCreate(tx *gorm.DB) error {
	m.ID = uuid.New()
	return nil
}

func (m *GvScore) BeforeCreate(tx *gorm.DB) error {
	m.ID = uuid.New()
	return nil
}

func (m *RelayStatus) BeforeCreate(tx *gorm.DB) error {
	m.ID = uuid.New()
	return nil
}

type RelayStatus struct {
	ID        uuid.UUID `gorm:"type:char(36);primary_key"`
	Url       string    `gorm:"size:512"`
	Status    string    `gorm:"size:512"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"`
	// change these defaults to something closer to zero
	LastEOSE       time.Time `gorm:"default:1970-01-01 00:00:00"`
	LastDisco      time.Time `gorm:"default:1970-01-01 00:00:00"`
	MetadataPubkey string    `gorm:"size:65"`
}

var TheLog *log.Logger
var DB *gorm.DB

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

func UpdateOrCreateRelayStatus(db *gorm.DB, url string, status string, pubkey string) {
	if pubkey == "" {
		var r []RelayStatus
		DB.Where("url = ?", url).Find(&r)

		for _, x := range r {
			UpdateOrCreateRelayStatus(DB, url, status, x.MetadataPubkey)
		}
	} else {
		var r RelayStatus
		if status == "connection established: EOSE" {
			r = RelayStatus{Url: url, Status: status, LastEOSE: time.Now(), MetadataPubkey: pubkey}
		} else if strings.HasPrefix(status, "connection error") {
			r = RelayStatus{Url: url, Status: status, LastDisco: time.Now(), MetadataPubkey: pubkey}
		} else {
			r = RelayStatus{Url: url, Status: status, MetadataPubkey: pubkey}
		}
		var s RelayStatus
		err := db.Model(&s).Where("url = ? and metadata_pubkey = ?", url, pubkey).First(&s).Error
		if err == nil {
			db.Model(&r).Where("url = ? and metadata_pubkey = ?", url, pubkey).Updates(&r)
		} else {
			db.Create(&r)
		}
	}
}
