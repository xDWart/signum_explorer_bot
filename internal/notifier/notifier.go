package notifier

import (
	"gorm.io/gorm"
	"signum_explorer_bot/internal/api/signum_api"
	"signum_explorer_bot/internal/database/models"
	"sync"
)

type Notifier struct {
	db *gorm.DB
	sync.RWMutex
	signumClient *signum_api.Client
	notifierCh   chan NotifierMessage
}

type NotifierMessage struct {
	ChatID  int64
	Message string
}

type MonitoredAccount struct {
	ChatID int64
	models.DbAccount
}

func NewNotifier(db *gorm.DB, signumClient *signum_api.Client, notifierCh chan NotifierMessage, wg *sync.WaitGroup, shutdownChannel chan interface{}) *Notifier {
	notifier := &Notifier{
		db:           db,
		signumClient: signumClient,
		notifierCh:   notifierCh,
	}
	wg.Add(1)
	go notifier.startListener(wg, shutdownChannel)
	return notifier
}
