package tangle

import (
	"time"
	"db"
	"github.com/dgraph-io/badger"
)

func periodicRequest() {
	ticker := time.NewTicker(pingInterval)
	for range ticker.C {
		db.Locker.Lock()
		db.Locker.Unlock()
		_ = db.DB.View(func(txn *badger.Txn) error {
			// Request pending
			outgoingQueue <- getMessage(nil, nil, false, txn)
			return nil
		})
	}
}

func periodicTipRequest() {
	ticker := time.NewTicker(tipPingInterval)
	for range ticker.C {
		db.Locker.Lock()
		db.Locker.Unlock()
		_ = db.DB.View(func(txn *badger.Txn) error {
			// Request tip
			outgoingQueue <- getMessage(nil, nil, true, txn)
			return nil
		})
	}
}

func requestIfMissing (hash []byte, addr string, txn *badger.Txn) bool {
	tx := txn
	has := true
	if txn == nil {
		tx = db.DB.NewTransaction(true)
		defer func () error {
			return tx.Commit(func(e error) {})
		}()
	}
	if !db.Has(db.GetByteKey(hash, db.KEY_HASH), tx) { //&& !db.Has(db.GetByteKey(hash, db.KEY_PENDING), tx) {
		db.Put(db.GetByteKey(hash, db.KEY_PENDING), int(time.Now().Unix()), nil, tx)
		db.Put(db.GetByteKey(hash, db.KEY_PENDING_HASH), hash, nil, tx)
		message := getMessage(nil, hash, false, tx)
		message.Addr = addr
		outgoingQueue <- message
		has = false
	}
	return has
}
