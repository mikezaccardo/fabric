/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fsblkstorage

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/core/ledger/blkstorage"
	"github.com/hyperledger/fabric/core/ledger/util"
	"github.com/hyperledger/fabric/core/ledger/util/db"
	"github.com/hyperledger/fabric/protos/common"
	pb "github.com/hyperledger/fabric/protos/peer"
	putil "github.com/hyperledger/fabric/protos/utils"
	"github.com/op/go-logging"
)

var logger = logging.MustGetLogger("kvledger")

const (
	blockIndexCF    = "blockIndexCF"
	blockfilePrefix = "blockfile_"
)

var (
	blkMgrInfoKey = []byte("blkMgrInfo")
)

type blockfileMgr struct {
	rootDir           string
	conf              *Conf
	db                *db.DB
	index             index
	cpInfo            *checkpointInfo
	cpInfoCond        *sync.Cond
	currentFileWriter *blockfileWriter
	bcInfo            atomic.Value
}

/*
Creates a new manager that will manage the files used for block persistence.
This manager manages the file system FS including
  -- the directory where the files are stored
  -- the individual files where the blocks are stored
  -- the checkpoint which tracks the latest file being persisted to
  -- the index which tracks what block and transaction is in what file
When a new blockfile manager is started (i.e. only on start-up), it checks
if this start-up is the first time the system is coming up or is this a restart
of the system.

The blockfile manager stores blocks of data into a file system.  That file
storage is done by creating sequentially numbered files of a configured size
i.e blockfile_000000, blockfile_000001, etc..

Each transcation in a block is stored with information about the number of
bytes in that transaction
 Adding txLoc [fileSuffixNum=0, offset=3, bytesLength=104] for tx [1:0] to index
 Adding txLoc [fileSuffixNum=0, offset=107, bytesLength=104] for tx [1:1] to index
Each block is stored with the total encoded length of that block as well as the
tx location offsets.

Remember that these steps are only done once at start-up of the system.
At start up a new manager:
  *) Checks if the directory for storing files exists, if not creates the dir
  *) Checks if the key value database exists, if not creates one
       (will create a db dir)
  *) Determines the checkpoint information (cpinfo) used for storage
		-- Loads from db if exist, if not instantiate a new cpinfo
		-- If cpinfo was loaded from db, compares to FS
		-- If cpinfo and file system are not in sync, syncs cpInfo from FS
  *) Starts a new file writer
		-- truncates file per cpinfo to remove any excess past last block
  *) Determines the index information used to find tx and blocks in
  the file blkstorage
		-- Instantiates a new blockIdxInfo
		-- Loads the index from the db if exists
		-- syncIndex comparing the last block indexed to what is in the FS
		-- If index and file system are not in sync, syncs index from the FS
  *)  Updates blockchain info used by the APIs
*/
func newBlockfileMgr(conf *Conf, indexConfig *blkstorage.IndexConfig) *blockfileMgr {
	//Determine the root directory for the blockfile storage, if it does not exist create it
	rootDir := conf.blockfilesDir
	_, err := util.CreateDirIfMissing(rootDir)
	if err != nil {
		panic(fmt.Sprintf("Error: %s", err))
	}
	//Determine the kev value db instance, if it does not exist, create the directory and instantiate the database.
	db := initDB(conf)
	// Instantiate the manager, i.e. blockFileMgr structure
	mgr := &blockfileMgr{rootDir: rootDir, conf: conf, db: db}

	// cp = checkpointInfo, retrieve from the database the file suffix or number of where blocks were stored.
	// It also retrieves the current size of that file and the last block number that was written to that file.
	// At init checkpointInfo:latestFileChunkSuffixNum=[0], latestFileChunksize=[0], lastBlockNumber=[0]
	cpInfo, err := mgr.loadCurrentInfo()
	if err != nil {
		panic(fmt.Sprintf("Could not get block file info for current block file from db: %s", err))
	}
	if cpInfo == nil { //if no cpInfo stored in db initiate to zero
		cpInfo = &checkpointInfo{latestFileChunkSuffixNum: 0, latestFileChunksize: 0}
		err = mgr.saveCurrentInfo(cpInfo, true)
		if err != nil {
			panic(fmt.Sprintf("Could not save next block file info to db: %s", err))
		}
	}
	//Verify that the checkpoint stored in db is accurate with what is actually stored in block file system
	// If not the same, sync the cpInfo and the file system
	syncCPInfoFromFS(conf, cpInfo)
	//Open a writer to the file identified by the number and truncate it to only contain the latest block
	// that was completely saved (file system, index, cpinfo, etc)
	currentFileWriter, err := newBlockfileWriter(deriveBlockfilePath(rootDir, cpInfo.latestFileChunkSuffixNum))
	if err != nil {
		panic(fmt.Sprintf("Could not open writer to current file: %s", err))
	}
	//Truncate the file to remove excess past last block
	err = currentFileWriter.truncateFile(cpInfo.latestFileChunksize)
	if err != nil {
		panic(fmt.Sprintf("Could not truncate current file to known size in db: %s", err))
	}

	// Create a new KeyValue store database handler for the blocks index in the keyvalue database
	mgr.index = newBlockIndex(indexConfig, db)

	// Update the manager with the checkpoint info and the file writer
	mgr.cpInfo = cpInfo
	mgr.currentFileWriter = currentFileWriter
	// Create a checkpoint condition (event) variable, for the  goroutine waiting for
	// or announcing the occurrence of an event.
	mgr.cpInfoCond = sync.NewCond(&sync.Mutex{})

	// Verify that the index stored in db is accurate with what is actually stored in block file system
	// If not the same, sync the index and the file system
	mgr.syncIndex()

	// init BlockchainInfo for external API's
	bcInfo := &pb.BlockchainInfo{
		Height:            0,
		CurrentBlockHash:  nil,
		PreviousBlockHash: nil}

	//If start up is a restart of an existing storage, update BlockchainInfo for external API's
	if cpInfo.lastBlockNumber > 0 {
		lastBlock, err := mgr.retrieveSerBlockByNumber(cpInfo.lastBlockNumber)
		if err != nil {
			panic(fmt.Sprintf("Could not retrieve last block form file: %s", err))
		}
		lastBlockHash := lastBlock.ComputeHash()
		previousBlockHash, err := lastBlock.GetPreviousBlockHash()
		if err != nil {
			panic(fmt.Sprintf("Error in decoding block: %s", err))
		}
		bcInfo = &pb.BlockchainInfo{
			Height:            cpInfo.lastBlockNumber,
			CurrentBlockHash:  lastBlockHash,
			PreviousBlockHash: previousBlockHash}
	}
	mgr.bcInfo.Store(bcInfo)
	//return the new manager (blockfileMgr)
	return mgr
}

func initDB(conf *Conf) *db.DB {
	dbInst := db.CreateDB(&db.Conf{
		DBPath: conf.dbPath})
	dbInst.Open()
	return dbInst
}

//cp = checkpointInfo, from the database gets the file suffix and the size of
// the file of where the last block was written.  Also retrieves contains the
// last block number that was written.  At init
//checkpointInfo:latestFileChunkSuffixNum=[0], latestFileChunksize=[0], lastBlockNumber=[0]
func syncCPInfoFromFS(conf *Conf, cpInfo *checkpointInfo) {
	logger.Debugf("Starting checkpoint=%s", cpInfo)
	//Checks if the file suffix of where the last block was written exists
	rootDir := conf.blockfilesDir
	filePath := deriveBlockfilePath(rootDir, cpInfo.latestFileChunkSuffixNum)
	exists, size, err := util.FileExists(filePath)
	if err != nil {
		panic(fmt.Sprintf("Error in checking whether file [%s] exists: %s", filePath, err))
	}
	logger.Debugf("status of file [%s]: exists=[%t], size=[%d]", filePath, exists, size)
	//Test is !exists because when file number is first used the file does not exist yet
	//checks that the file exists and that the size of the file is what is stored in cpinfo
	//status of file [/tmp/tests/ledger/blkstorage/fsblkstorage/blocks/blockfile_000000]: exists=[false], size=[0]
	if !exists || int(size) == cpInfo.latestFileChunksize {
		// check point info is in sync with the file on disk
		return
	}
	//Scan the file system to verify that the checkpoint info stored in db is correct
	endOffsetLastBlock, numBlocks, err := scanForLastCompleteBlock(
		rootDir, cpInfo.latestFileChunkSuffixNum, int64(cpInfo.latestFileChunksize))
	if err != nil {
		panic(fmt.Sprintf("Could not open current file for detecting last block in the file: %s", err))
	}
	//Updates the checkpoint info for the actual last block number stored and it's end location
	cpInfo.lastBlockNumber += uint64(numBlocks)
	cpInfo.latestFileChunksize = int(endOffsetLastBlock)
	logger.Debugf("Checkpoint after updates by scanning the last file segment:%s", cpInfo)
}

func deriveBlockfilePath(rootDir string, suffixNum int) string {
	return rootDir + "/" + blockfilePrefix + fmt.Sprintf("%06d", suffixNum)
}

func (mgr *blockfileMgr) open() error {
	return mgr.currentFileWriter.open()
}

func (mgr *blockfileMgr) close() {
	mgr.currentFileWriter.close()
	mgr.db.Close()
}

func (mgr *blockfileMgr) moveToNextFile() {
	cpInfo := &checkpointInfo{
		latestFileChunkSuffixNum: mgr.cpInfo.latestFileChunkSuffixNum + 1,
		latestFileChunksize:      0,
		lastBlockNumber:          mgr.cpInfo.lastBlockNumber}

	nextFileWriter, err := newBlockfileWriter(
		deriveBlockfilePath(mgr.rootDir, cpInfo.latestFileChunkSuffixNum))

	if err != nil {
		panic(fmt.Sprintf("Could not open writer to next file: %s", err))
	}
	mgr.currentFileWriter.close()
	err = mgr.saveCurrentInfo(cpInfo, true)
	if err != nil {
		panic(fmt.Sprintf("Could not save next block file info to db: %s", err))
	}
	mgr.currentFileWriter = nextFileWriter
	mgr.updateCheckpoint(cpInfo)
}

func (mgr *blockfileMgr) addBlock(block *pb.Block2) error {
	serBlock, err := pb.ConstructSerBlock2(block)
	if err != nil {
		return fmt.Errorf("Error while serializing block: %s", err)
	}
	blockBytes := serBlock.GetBytes()
	blockHash := serBlock.ComputeHash()
	//Get the location / offset where each transaction starts in the block and where the block ends
	txOffsets, err := serBlock.GetTxOffsets()
	currentOffset := mgr.cpInfo.latestFileChunksize
	if err != nil {
		return fmt.Errorf("Error while serializing block: %s", err)
	}
	blockBytesLen := len(blockBytes)
	blockBytesEncodedLen := proto.EncodeVarint(uint64(blockBytesLen))
	totalBytesToAppend := blockBytesLen + len(blockBytesEncodedLen)

	//Determine if we need to start a new file since the size of this block
	//exceeds the amount of space left in the current file
	if currentOffset+totalBytesToAppend > mgr.conf.maxBlockfileSize {
		mgr.moveToNextFile()
		currentOffset = 0
	}
	//append blockBytesEncodedLen to the file
	err = mgr.currentFileWriter.append(blockBytesEncodedLen, false)
	if err == nil {
		//append the actual block bytes to the file
		err = mgr.currentFileWriter.append(blockBytes, true)
	}
	if err != nil {
		truncateErr := mgr.currentFileWriter.truncateFile(mgr.cpInfo.latestFileChunksize)
		if truncateErr != nil {
			panic(fmt.Sprintf("Could not truncate current file to known size after an error during block append: %s", err))
		}
		return fmt.Errorf("Error while appending block to file: %s", err)
	}

	//Update the checkpoint info with the results of adding the new block
	currentCPInfo := mgr.cpInfo
	newCPInfo := &checkpointInfo{
		latestFileChunkSuffixNum: currentCPInfo.latestFileChunkSuffixNum,
		latestFileChunksize:      currentCPInfo.latestFileChunksize + totalBytesToAppend,
		lastBlockNumber:          currentCPInfo.lastBlockNumber + 1}
	//save the checkpoint information in the database
	if err = mgr.saveCurrentInfo(newCPInfo, false); err != nil {
		truncateErr := mgr.currentFileWriter.truncateFile(currentCPInfo.latestFileChunksize)
		if truncateErr != nil {
			panic(fmt.Sprintf("Error in truncating current file to known size after an error in saving checkpoint info: %s", err))
		}
		return fmt.Errorf("Error while saving current file info to db: %s", err)
	}

	//Index block file location pointer updated with file suffex and offset for the new block
	blockFLP := &fileLocPointer{fileSuffixNum: newCPInfo.latestFileChunkSuffixNum}
	blockFLP.offset = currentOffset
	// shift the txoffset because we prepend length of bytes before block bytes
	for i := 0; i < len(txOffsets); i++ {
		txOffsets[i] += len(blockBytesEncodedLen)
	}
	//save the index in the database
	mgr.index.indexBlock(&blockIdxInfo{
		blockNum: newCPInfo.lastBlockNumber, blockHash: blockHash,
		flp: blockFLP, txOffsets: txOffsets})

	//update the checkpoint info (for storage) and the blockchain info (for APIs) in the manager
	mgr.updateCheckpoint(newCPInfo)
	mgr.updateBlockchainInfo(blockHash, block)
	return nil
}

func (mgr *blockfileMgr) syncIndex() error {
	var lastBlockIndexed uint64
	var err error
	//from the database, get the last block that was indexed
	if lastBlockIndexed, err = mgr.index.getLastBlockIndexed(); err != nil {
		return err
	}
	//initialize index to file number:zero, offset:zero and block:1
	startFileNum := 0
	startOffset := 0
	blockNum := uint64(1)
	//get the last file that blocks were added to using the checkpoint info
	endFileNum := mgr.cpInfo.latestFileChunkSuffixNum
	//if the index stored in the db has value, update the index information with those values
	if lastBlockIndexed != 0 {
		var flp *fileLocPointer
		if flp, err = mgr.index.getBlockLocByBlockNum(lastBlockIndexed); err != nil {
			return err
		}
		startFileNum = flp.fileSuffixNum
		startOffset = flp.locPointer.offset
		blockNum = lastBlockIndexed
	}

	//open a blockstream to the file location that was stored in the index
	var stream *blockStream
	if stream, err = newBlockStream(mgr.rootDir, startFileNum, int64(startOffset), endFileNum); err != nil {
		return err
	}
	var blockBytes []byte
	var blockPlacementInfo *blockPlacementInfo

	//Should be at the last block, but go ahead and loop looking for next blockBytes
	//If there is another block, add it to the index
	for {
		if blockBytes, blockPlacementInfo, err = stream.nextBlockBytesAndPlacementInfo(); err != nil {
			return err
		}
		if blockBytes == nil {
			break
		}
		serBlock2 := pb.NewSerBlock2(blockBytes)
		var txOffsets []int
		if txOffsets, err = serBlock2.GetTxOffsets(); err != nil {
			return err
		}
		for i := 0; i < len(txOffsets); i++ {
			txOffsets[i] += int(blockPlacementInfo.blockBytesOffset)
		}
		//Update the blockIndexInfo with what was actually stored in file system
		blockIdxInfo := &blockIdxInfo{}
		blockIdxInfo.blockHash = serBlock2.ComputeHash()
		blockIdxInfo.blockNum = blockNum
		blockIdxInfo.flp = &fileLocPointer{fileSuffixNum: blockPlacementInfo.fileNum,
			locPointer: locPointer{offset: int(blockPlacementInfo.blockStartOffset)}}
		blockIdxInfo.txOffsets = txOffsets
		if err = mgr.index.indexBlock(blockIdxInfo); err != nil {
			return err
		}
		blockNum++
	}
	return nil
}

func (mgr *blockfileMgr) getBlockchainInfo() *pb.BlockchainInfo {
	return mgr.bcInfo.Load().(*pb.BlockchainInfo)
}

func (mgr *blockfileMgr) updateCheckpoint(cpInfo *checkpointInfo) {
	mgr.cpInfoCond.L.Lock()
	defer mgr.cpInfoCond.L.Unlock()
	mgr.cpInfo = cpInfo
	logger.Debugf("Broadcasting about update checkpointInfo: %s", cpInfo)
	mgr.cpInfoCond.Broadcast()
}

func (mgr *blockfileMgr) updateBlockchainInfo(latestBlockHash []byte, latestBlock *pb.Block2) {
	currentBCInfo := mgr.getBlockchainInfo()
	newBCInfo := &pb.BlockchainInfo{
		Height:            currentBCInfo.Height + 1,
		CurrentBlockHash:  latestBlockHash,
		PreviousBlockHash: latestBlock.PreviousBlockHash}

	mgr.bcInfo.Store(newBCInfo)
}

func (mgr *blockfileMgr) retrieveBlockByHash(blockHash []byte) (*pb.Block2, error) {
	logger.Debugf("retrieveBlockByHash() - blockHash = [%#v]", blockHash)
	loc, err := mgr.index.getBlockLocByHash(blockHash)
	if err != nil {
		return nil, err
	}
	return mgr.fetchBlock(loc)
}

func (mgr *blockfileMgr) retrieveBlockByNumber(blockNum uint64) (*pb.Block2, error) {
	logger.Debugf("retrieveBlockByNumber() - blockNum = [%d]", blockNum)
	loc, err := mgr.index.getBlockLocByBlockNum(blockNum)
	if err != nil {
		return nil, err
	}
	return mgr.fetchBlock(loc)
}

func (mgr *blockfileMgr) retrieveSerBlockByNumber(blockNum uint64) (*pb.SerBlock2, error) {
	logger.Debugf("retrieveSerBlockByNumber() - blockNum = [%d]", blockNum)
	loc, err := mgr.index.getBlockLocByBlockNum(blockNum)
	if err != nil {
		return nil, err
	}
	return mgr.fetchSerBlock(loc)
}

func (mgr *blockfileMgr) retrieveBlocks(startNum uint64) (*BlocksItr, error) {
	return newBlockItr(mgr, startNum), nil
}

func (mgr *blockfileMgr) retrieveTransactionByID(txID string) (*pb.Transaction, error) {
	logger.Debugf("retrieveTransactionByID() - txId = [%s]", txID)
	loc, err := mgr.index.getTxLoc(txID)
	if err != nil {
		return nil, err
	}
	return mgr.fetchTransaction(loc)
}

func (mgr *blockfileMgr) fetchBlock(lp *fileLocPointer) (*pb.Block2, error) {
	serBlock, err := mgr.fetchSerBlock(lp)
	if err != nil {
		return nil, err
	}
	block, err := serBlock.ToBlock2()
	if err != nil {
		return nil, err
	}
	return block, nil
}

func (mgr *blockfileMgr) fetchSerBlock(lp *fileLocPointer) (*pb.SerBlock2, error) {
	blockBytes, err := mgr.fetchBlockBytes(lp)
	if err != nil {
		return nil, err
	}
	return pb.NewSerBlock2(blockBytes), nil
}

func (mgr *blockfileMgr) fetchTransaction(lp *fileLocPointer) (*pb.Transaction, error) {
	var err error
	var txEnvelopeBytes []byte
	if txEnvelopeBytes, err = mgr.fetchRawBytes(lp); err != nil {
		return nil, err
	}
	return extractTransaction(txEnvelopeBytes)
}

func (mgr *blockfileMgr) fetchBlockBytes(lp *fileLocPointer) ([]byte, error) {
	stream, err := newBlockfileStream(mgr.rootDir, lp.fileSuffixNum, int64(lp.offset))
	if err != nil {
		return nil, err
	}
	defer stream.close()
	b, err := stream.nextBlockBytes()
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (mgr *blockfileMgr) fetchRawBytes(lp *fileLocPointer) ([]byte, error) {
	filePath := deriveBlockfilePath(mgr.rootDir, lp.fileSuffixNum)
	reader, err := newBlockfileReader(filePath)
	if err != nil {
		return nil, err
	}
	defer reader.close()
	b, err := reader.read(lp.offset, lp.bytesLength)
	if err != nil {
		return nil, err
	}
	return b, nil
}

//Get the current checkpoint information that is stored in the database
func (mgr *blockfileMgr) loadCurrentInfo() (*checkpointInfo, error) {
	var b []byte
	var err error
	if b, err = mgr.db.Get(blkMgrInfoKey); b == nil || err != nil {
		return nil, err
	}
	i := &checkpointInfo{}
	if err = i.unmarshal(b); err != nil {
		return nil, err
	}
	logger.Debugf("loaded checkpointInfo:%s", i)
	return i, nil
}

func (mgr *blockfileMgr) saveCurrentInfo(i *checkpointInfo, sync bool) error {
	b, err := i.marshal()
	if err != nil {
		return err
	}
	if err = mgr.db.Put(blkMgrInfoKey, b, sync); err != nil {
		return err
	}
	return nil
}

// scanForLastCompleteBlock scan a given block file and detects the last offset in the file
// after which there may lie a block partially written (towards the end of the file in a crash scenario).
func scanForLastCompleteBlock(rootDir string, fileNum int, startingOffset int64) (int64, int, error) {
	//scan the passed file number suffix starting from the passed offset to find the last completed block
	numBlocks := 0
	blockStream, errOpen := newBlockfileStream(rootDir, fileNum, startingOffset)
	if errOpen != nil {
		return 0, 0, errOpen
	}
	defer blockStream.close()
	var errRead error
	var blockBytes []byte
	for {
		blockBytes, errRead = blockStream.nextBlockBytes()
		if blockBytes == nil || errRead != nil {
			break
		}
		numBlocks++
	}
	if errRead == ErrUnexpectedEndOfBlockfile {
		logger.Debugf(`Error:%s
		The error may happen if a crash has happened during block appending.
		Resetting error to nil and returning current offset as a last complete block's end offset`, errRead)
		errRead = nil
	}
	logger.Debugf("scanForLastCompleteBlock(): last complete block ends at offset=[%d]", blockStream.currentOffset)
	return blockStream.currentOffset, numBlocks, errRead
}

func extractTransaction(txEnvelopeBytes []byte) (*pb.Transaction, error) {
	var err error
	var txEnvelope *common.Envelope
	var txPayload *common.Payload
	var tx *pb.Transaction

	if txEnvelope, err = putil.GetEnvelope(txEnvelopeBytes); err != nil {
		return nil, err
	}
	if txPayload, err = putil.GetPayload(txEnvelope); err != nil {
		return nil, err
	}
	if tx, err = putil.GetTransaction(txPayload.Data); err != nil {
		return nil, err
	}
	return tx, nil
}

// checkpointInfo
type checkpointInfo struct {
	latestFileChunkSuffixNum int
	latestFileChunksize      int
	lastBlockNumber          uint64
}

func (i *checkpointInfo) marshal() ([]byte, error) {
	buffer := proto.NewBuffer([]byte{})
	var err error
	if err = buffer.EncodeVarint(uint64(i.latestFileChunkSuffixNum)); err != nil {
		return nil, err
	}
	if err = buffer.EncodeVarint(uint64(i.latestFileChunksize)); err != nil {
		return nil, err
	}
	if err = buffer.EncodeVarint(i.lastBlockNumber); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func (i *checkpointInfo) unmarshal(b []byte) error {
	buffer := proto.NewBuffer(b)
	var val uint64
	var err error

	if val, err = buffer.DecodeVarint(); err != nil {
		return err
	}
	i.latestFileChunkSuffixNum = int(val)

	if val, err = buffer.DecodeVarint(); err != nil {
		return err
	}
	i.latestFileChunksize = int(val)

	if val, err = buffer.DecodeVarint(); err != nil {
		return err
	}
	i.lastBlockNumber = val

	return nil
}

func (i *checkpointInfo) String() string {
	return fmt.Sprintf("latestFileChunkSuffixNum=[%d], latestFileChunksize=[%d], lastBlockNumber=[%d]",
		i.latestFileChunkSuffixNum, i.latestFileChunksize, i.lastBlockNumber)
}
