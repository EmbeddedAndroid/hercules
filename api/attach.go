package api

// complaints or suggestions pls to pmaxuw on discord

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"../logs"

	"github.com/gin-gonic/gin"
	"github.com/iotaledger/giota"
	"github.com/muxxer/powsrv"
	"github.com/spf13/viper"
)

const (
	// not defined in giota library
	MaxTimestampValue = 3812798742493 //int64(3^27 - 1) / 2
)

var mutex = &sync.Mutex{}
var maxMinWeightMagnitude = 0
var maxTransactions = 0
var usePowSrv = false
var powClient *powsrv.PowClient
var interruptAttachToTangle = false
var powInitialized = false
var powFunc giota.PowFunc
var powType string
var powVersion string
var serverVersion string

func init() {
	addStartModule(startAttach)

	addAPICall("attachToTangle", attachToTangle)
	addAPICall("interruptAttachingToTangle", interruptAttachingToTangle)
}

func startAttach(apiConfig *viper.Viper) {
	maxMinWeightMagnitude = config.GetInt("api.pow.maxMinWeightMagnitude")
	maxTransactions = config.GetInt("api.pow.maxTransactions")
	usePowSrv = config.GetBool("api.pow.usePowSrv")

	logs.Log.Debug("maxMinWeightMagnitude:", maxMinWeightMagnitude)
	logs.Log.Debug("maxTransactions:", maxTransactions)
	logs.Log.Debug("usePowSrv:", usePowSrv)

	if usePowSrv {
		powClient = &powsrv.PowClient{PowSrvPath: config.GetString("api.pow.powSrvPath"), WriteTimeOutMs: 500, ReadTimeOutMs: 120000}
	}
}

// TODO: maybe the trytes/trits/runes conversions should be ported to the version in giota library
// The project still used home-brew conversions and types
func IsValidPoW(hash giota.Trits, mwm int) bool {
	for i := len(hash) - mwm; i < len(hash); i++ {
		if hash[i] != 0 {
			return false
		}
	}
	return true
}

func toRunesCheckTrytes(s string, length int) ([]rune, error) {
	if len(s) != length {
		return []rune{}, errors.New("invalid length")
	}
	if _, err := giota.ToTrytes(s); err != nil {
		return []rune{}, err
	}
	return []rune(string(s)), nil
}

func toRunes(t giota.Trytes) []rune {
	return []rune(string(t))
}

// interrupts not PoW itselfe (no PoW of giota support interrupts) but stops
// attatchToTangle after the last transaction PoWed
func interruptAttachingToTangle(request Request, c *gin.Context, t time.Time) {
	interruptAttachToTangle = true
	c.JSON(http.StatusOK, gin.H{})
}

func getTimestampMilliseconds() int64 {
	return time.Now().UnixNano() / (int64(time.Millisecond) / int64(time.Nanosecond)) // time.Nanosecond should always be 1 ... but if not ...^^
}

// attachToTangle
// do everything with trytes and save time by not convertig to trits and back
// all constants have to be divided by 3
func attachToTangle(request Request, c *gin.Context, t time.Time) {
	// only one attatchToTangle allowed in parallel
	mutex.Lock()
	defer mutex.Unlock()

	interruptAttachToTangle = false

	var returnTrytes []string

	trunkTransaction, err := toRunesCheckTrytes(request.TrunkTransaction, giota.TrunkTransactionTrinarySize/3)
	if err != nil {
		ReplyError("Invalid trunkTransaction-Trytes", c)
		return
	}

	branchTransaction, err := toRunesCheckTrytes(request.BranchTransaction, giota.BranchTransactionTrinarySize/3)
	if err != nil {
		ReplyError("Invalid branchTransaction-Trytes", c)
		return
	}

	minWeightMagnitude := request.MinWeightMagnitude

	// restrict minWeightMagnitude
	if minWeightMagnitude > maxMinWeightMagnitude {
		ReplyError("MinWeightMagnitude too high", c)
		return
	}

	trytes := request.Trytes

	// limit number of transactions in a bundle
	if len(trytes) > maxTransactions {
		ReplyError("Too many transactions", c)
		return
	}
	returnTrytes = make([]string, len(trytes))
	inputRunes := make([][]rune, len(trytes))

	// validate input trytes before doing PoW
	for idx, tryte := range trytes {
		if runes, err := toRunesCheckTrytes(tryte, giota.TransactionTrinarySize/3); err != nil {
			ReplyError("Error in Tryte input", c)
			return
		} else {
			inputRunes[idx] = runes
		}
	}

	var prevTransaction []rune

	if !powInitialized {
		if usePowSrv {
			serverVersion, powType, powVersion, err = powClient.GetPowInfo()
			if err != nil {
				ReplyError(err.Error(), c)
				return
			}

			powFunc = powClient.PowFunc
		} else {
			powType, powFunc = giota.GetBestPoW()
		}
		powInitialized = true
	}

	if usePowSrv {
		logs.Log.Debugf("[PoW] Using powSrv version \"%v\"", serverVersion)
		logs.Log.Debugf("[PoW] Best method \"%v\"", powType)
		if powVersion != "" {
			logs.Log.Debugf("[PoW] Version \"%v\"", powVersion)
		}
	} else {
		logs.Log.Debugf("[PoW] Best method \"%v\"", powType)
	}

	// do pow
	for idx, runes := range inputRunes {
		if interruptAttachToTangle {
			ReplyError("attatchToTangle interrupted", c)
			return
		}
		timestamp := getTimestampMilliseconds()
		//branch and trunk
		tmp := prevTransaction
		if len(prevTransaction) == 0 {
			tmp = trunkTransaction
		}
		copy(runes[giota.TrunkTransactionTrinaryOffset/3:], tmp[:giota.TrunkTransactionTrinarySize/3])

		tmp = trunkTransaction
		if len(prevTransaction) == 0 {
			tmp = branchTransaction
		}
		copy(runes[giota.BranchTransactionTrinaryOffset/3:], tmp[:giota.BranchTransactionTrinarySize/3])

		//attachment fields: tag and timestamps
		//tag - copy the obsolete tag to the attachment tag field only if tag isn't set.
		if string(runes[giota.TagTrinaryOffset/3:(giota.TagTrinaryOffset+giota.TagTrinarySize)/3]) == "999999999999999999999999999" {
			copy(runes[giota.TagTrinaryOffset/3:], runes[giota.ObsoleteTagTrinaryOffset/3:(giota.ObsoleteTagTrinaryOffset+giota.ObsoleteTagTrinarySize)/3])
		}

		runesTimeStamp := toRunes(giota.Int2Trits(timestamp, giota.AttachmentTimestampTrinarySize).Trytes())
		runesTimeStampLowerBoundary := toRunes(giota.Int2Trits(0, giota.AttachmentTimestampLowerBoundTrinarySize).Trytes())
		runesTimeStampUpperBoundary := toRunes(giota.Int2Trits(MaxTimestampValue, giota.AttachmentTimestampUpperBoundTrinarySize).Trytes())

		copy(runes[giota.AttachmentTimestampTrinaryOffset/3:], runesTimeStamp[:giota.AttachmentTimestampTrinarySize/3])
		copy(runes[giota.AttachmentTimestampLowerBoundTrinaryOffset/3:], runesTimeStampLowerBoundary[:giota.AttachmentTimestampLowerBoundTrinarySize/3])
		copy(runes[giota.AttachmentTimestampUpperBoundTrinaryOffset/3:], runesTimeStampUpperBoundary[:giota.AttachmentTimestampUpperBoundTrinarySize/3])

		startTime := time.Now()
		nonceTrytes, err := powFunc(giota.Trytes(runes), minWeightMagnitude)
		if err != nil || len(nonceTrytes) != giota.NonceTrinarySize/3 {
			ReplyError(fmt.Sprintf("PoW failed! %v", err.Error()), c)
			return
		}
		elapsedTime := time.Now().Sub(startTime)
		logs.Log.Debug("[PoW] Needed", elapsedTime)

		// copy nonce to runes
		copy(runes[giota.NonceTrinaryOffset/3:], toRunes(nonceTrytes)[:giota.NonceTrinarySize/3])

		verifyTrytes, err := giota.ToTrytes(string(runes))
		if err != nil {
			ReplyError("Trytes got corrupted", c)
			return
		}

		//validate PoW - throws exception if invalid
		hash := verifyTrytes.Hash()
		if !IsValidPoW(hash.Trits(), minWeightMagnitude) {
			ReplyError("Nonce verify failed", c)
			return
		}

		logs.Log.Debug("[PoW] Verified!")

		returnTrytes[idx] = string(runes)

		prevTransaction = toRunes(hash)
	}

	c.JSON(http.StatusOK, gin.H{
		"trytes":   returnTrytes,
		"duration": getDuration(t),
	})
}
