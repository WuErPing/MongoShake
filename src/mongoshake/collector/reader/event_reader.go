package sourceReader

// read change stream event from source mongodb

import (
	"sync"
	"time"

	"mongoshake/diskQueue"
	"mongoshake/common"
	"mongoshake/collector/configure"

	"github.com/vinllen/mgo/bson"
	LOG "github.com/vinllen/log4go"
	"sync/atomic"
	"mongoshake/oplog"
	"go.mongodb.org/mongo-driver/mongo"
)

type EventReader struct {
	// source mongo address url
	src     string
	replset string

	// mongo client
	client *utils.ChangeStreamConn

	// stage of fetch and store oplog
	fetchStage int32
	// disk queue used to store oplog temporarily
	diskQueue     *diskQueue.DiskQueue
	disQueueMutex sync.Mutex // disk queue mutex

	// start at operation time
	startAtOperationTime int64

	// event channel
	eventChan    chan *retOplog
	fetcherExist bool
	fetcherLock  sync.Mutex

	firstRead       bool
	diskQueueLastTs bson.MongoTimestamp // the last oplog timestamp in disk queue
}

// NewEventReader creates reader with mongodb url
func NewEventReader(src string, replset string) *EventReader {
	return &EventReader{
		src:                  src,
		replset:              replset,
		startAtOperationTime: -1, // init value
		eventChan:            make(chan *retOplog, oplogChanSize),
		firstRead:            true,
		diskQueueLastTs:      -1,
	}
}

func (er *EventReader) InitDiskQueue(dqName string) {
	fetchStage := er.fetchStage
	if fetchStage != utils.FetchStageStoreDiskNoApply && fetchStage != utils.FetchStageStoreDiskApply {
		LOG.Crashf("event_reader replset %v init disk queue in illegal fetchStage %v",
			er.replset, utils.LogFetchStage(fetchStage))
	}
	er.diskQueue = diskQueue.NewDiskQueue(dqName, conf.Options.LogDirectory,
		conf.Options.FullSyncOplogStoreDiskMaxSize, ReplayerOplogStoreDiskReadBatch,
		1 << 30, 0, 1 << 26,
		1000, 2 * time.Second)
}

func (er *EventReader) UpdateFetchStage(fetchStage int32) {
	LOG.Info("event_reader replset[%v] update fetch status to: %v", er.replset, utils.LogFetchStage(fetchStage))
	atomic.StoreInt32(&er.fetchStage, fetchStage)
}

func (er *EventReader) GetDiskQueueName() string {
	return er.diskQueue.Name()
}

func (er *EventReader) GetQueryTsFromDiskQueue() bson.MongoTimestamp {
	if er.diskQueue == nil {
		LOG.Crashf("event_reader replset %v get query timestamp from nil disk queue", er.replset)
	}
	logData := er.diskQueue.GetLastWriteData()
	if len(logData) == 0 {
		return 0
	}
	log := new(oplog.Event)
	if err := bson.Unmarshal(logData, log); err != nil {
		LOG.Crashf("unmarshal oplog[%v] failed[%v]", logData, err)
	}
	return log.ClusterTime
}

// SetQueryTimestampOnEmpty set internal timestamp if
// not exist in this or. initial stage most of the time
func (er *EventReader) SetQueryTimestampOnEmpty(ts bson.MongoTimestamp) {
	if er.startAtOperationTime == -1 {
		er.startAtOperationTime = utils.TimestampToInt64(ts)
	}
}

func (er *EventReader) UpdateQueryTimestamp(ts bson.MongoTimestamp) {
	er.startAtOperationTime = utils.TimestampToInt64(ts)
}

func (er *EventReader) getQueryTimestamp() bson.MongoTimestamp {
	return bson.MongoTimestamp(er.startAtOperationTime)
}

// Next returns an oplog by raw bytes which is []byte
func (er *EventReader) Next() ([]byte, error) {
	return er.get()
}

func (er *EventReader) get() ([]byte, error) {
	select {
	case ret := <-er.eventChan:
		return ret.log, ret.err
	case <-time.After(time.Second * time.Duration(conf.Options.SyncerReaderBufferTime)):
		return nil, TimeoutError
	}
}

// start fetcher if not exist
func (er *EventReader) StartFetcher() {
	if er.fetcherExist == true {
		return
	}

	er.fetcherLock.Lock()
	if er.fetcherExist == false { // double check
		er.fetcherExist = true
		go er.fetcher()
		fetchStage := atomic.LoadInt32(&er.fetchStage)
		if fetchStage == utils.FetchStageStoreDiskNoApply || fetchStage == utils.FetchStageStoreDiskApply {
			go er.retrieve()
		}
	}
	er.fetcherLock.Unlock()
}

// fetch change stream event tp store disk queue or memory
func (er *EventReader) fetcher() {
	for {
		if err := er.EnsureNetwork(); err != nil {
			er.eventChan <- &retOplog{nil, err}
			continue
		}

		ok, data := er.client.GetNext()
		if !ok {
			// no data
			er.client.Close()
			LOG.Error("change stream reader hit the end")
			time.Sleep(1 * time.Second)
		}

		er.eventChan <-
	}
}

func (er *EventReader) retrieve() {

}

func (er *EventReader) EnsureNetwork() error {
	if er.client != nil && er.client.IsNotNil() {
		return nil
	}

	if er.client != nil {
		er.client.Close() // close old client
	}

	var err error
	if er.client, err = utils.NewChangeStreamConn(er.src, er.startAtOperationTime); err != nil {
		return err
	}

	return nil
}
