package urchin_file

import (
	"bytes"
	"context"
	commonv1 "d7y.io/api/pkg/apis/common/v1"
	"d7y.io/dragonfly/v2/client/config"
	"d7y.io/dragonfly/v2/client/daemon/urchin_dataset"
	"d7y.io/dragonfly/v2/client/daemon/urchin_dataset_vesion"
	"d7y.io/dragonfly/v2/client/util"
	"encoding/json"
	"math/rand"

	"d7y.io/dragonfly/v2/client/daemon/peer"
	"d7y.io/dragonfly/v2/client/daemon/storage"
	urchinstatus "d7y.io/dragonfly/v2/client/daemon/urchin_status"
	urchintask "d7y.io/dragonfly/v2/client/daemon/urchin_task"
	logger "d7y.io/dragonfly/v2/internal/dflog"
	"d7y.io/dragonfly/v2/pkg/dfpath"
	"d7y.io/dragonfly/v2/pkg/digest"
	"d7y.io/dragonfly/v2/pkg/idgen"
	nethttp "d7y.io/dragonfly/v2/pkg/net/http"
	"d7y.io/dragonfly/v2/pkg/objectstorage"
	"d7y.io/dragonfly/v2/pkg/retry"
	pkgstrings "d7y.io/dragonfly/v2/pkg/strings"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/go-http-utils/headers"
	"github.com/opentracing/opentracing-go/log"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultFileMode      = os.FileMode(0644)
	defaultDirectoryMode = os.FileMode(0755)
)

const (
	//Source to compatible urchin1
	Source = iota
	Meta
	// Ephemeral only writes the object to the dfdaemon.
	// It is only provided for creating temporary objects between peers,
	// and users are not allowed to use this mode.
	Ephemeral
	Chunk
	ChunkEnd
	Blob
	ReplicaMetaOS
	ReplicaBlobOS
)

const (
	SeedPeerHostTtl = 20 * time.Minute
)

var (
	DataModeJson2Enum = map[string]int32{
		"Source":      Source,
		"Meta":        Meta,
		"Ephemeral":   Ephemeral,
		"Chunk":       Chunk,
		"ChunkEnd":    ChunkEnd,
		"Blob":        Blob,
		"ReplicaMeta": ReplicaMetaOS,
		"ReplicaBlob": ReplicaBlobOS,
	}

	DataModeEnum2Json = map[int32]string{
		Source:        "Source",
		Meta:          "Meta",
		Ephemeral:     "Ephemeral",
		Chunk:         "Chunk",
		ChunkEnd:      "ChunkEnd",
		Blob:          "Blob",
		ReplicaMetaOS: "ReplicaMeta",
		ReplicaBlobOS: "ReplicaBlob",
	}
)

const (
	// defaultSignExpireTime is default expire of sign url.
	defaultSignExpireTime = 2 * 60 * time.Minute
)

type UrchinFileManager struct {
	config            *config.DaemonOption
	dynconfig         config.Dynconfig
	peerTaskManager   peer.TaskManager
	storageManager    storage.Manager
	peerIDGenerator   peer.IDGenerator
	urchinTaskManager *urchintask.UrchinTaskManager
	cachingTasks      sync.Map
	cachingTaskLock   sync.Locker
}

func NewUrchinFileManager(config *config.DaemonOption, dynconfig config.Dynconfig,
	peerTaskManager peer.TaskManager, storageManager storage.Manager,
	peerIDGenerator peer.IDGenerator,
	urchinTaskManager *urchintask.UrchinTaskManager) (*UrchinFileManager, error) {
	return &UrchinFileManager{
		config:            config,
		dynconfig:         dynconfig,
		peerTaskManager:   peerTaskManager,
		storageManager:    storageManager,
		peerIDGenerator:   peerIDGenerator,
		urchinTaskManager: urchinTaskManager,
		cachingTasks:      sync.Map{},
		cachingTaskLock:   &sync.Mutex{},
	}, nil
}

func (urfm *UrchinFileManager) genMetaFileName(versionId string, versionDigest string) string {
	return fmt.Sprintf("%s_%s_%s", "meta", versionDigest, versionId)
}

func (urfm *UrchinFileManager) genBackendMetaFileObjectKey(datasetId string, versionId string, versionDigest string) string {
	datasetMetaFilePath := fmt.Sprintf("%s/%s", datasetId, urfm.genMetaFileName(versionId, versionDigest))
	return datasetMetaFilePath
}

func (urfm *UrchinFileManager) genLocalBlobFileParentDir(datasetId string) string {
	dataDir := dfpath.DefaultDataDir
	if urfm.config.DataDir != "" {
		dataDir = urfm.config.DataDir
	}

	datasetBlobFileParentDir := path.Join(dataDir, "blobs", datasetId)

	return datasetBlobFileParentDir
}

func (urfm *UrchinFileManager) genBlobFileName(versionId string, versionDigest string) string {
	return fmt.Sprintf("%s_%s_%s", "blob", versionDigest, versionId)
}

func (urfm *UrchinFileManager) genLocalBlobFilePath(datasetId string, versionId string, versionDigest string) string {
	datasetBlobFilePath := path.Join(urfm.genLocalBlobFileParentDir(datasetId), urfm.genBlobFileName(versionId, versionDigest))
	return datasetBlobFilePath
}

func (urfm *UrchinFileManager) genBackendBlobFileObjectKey(datasetId string, versionId string, versionDigest string) string {
	datasetBlobFilePath := fmt.Sprintf("%s/%s", datasetId, urfm.genBlobFileName(versionId, versionDigest))
	return datasetBlobFilePath
}

func (urfm *UrchinFileManager) genLocalChunkFileName(chunkNum uint64, chunkStart uint64) string {
	return fmt.Sprintf("%s-%s", strconv.FormatUint(chunkNum, 10), strconv.FormatUint(chunkStart, 10))
}

func (urfm *UrchinFileManager) genLocalChunkFilePath(chunksParentDir string, chunkNum uint64, chunkStart uint64) string {
	chunkFilePath := path.Join(chunksParentDir, urfm.genLocalChunkFileName(chunkNum, chunkStart))
	return chunkFilePath
}

func (urfm *UrchinFileManager) genLocalChunksParentDir(datasetId string, versionId string, versionDigest string) string {
	dataDir := dfpath.DefaultDataDir
	if urfm.config.DataDir != "" {
		dataDir = urfm.config.DataDir
	}

	chunksParentDir := path.Join(dataDir, "chunks", datasetId, urfm.genBlobFileName(versionId, versionDigest))
	return chunksParentDir
}

// UploadFile uses to upload object data.
func (urfm *UrchinFileManager) UploadFile(ctx *gin.Context) {
	var form UploadFileRequest
	if err := ctx.ShouldBind(&form); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	//ToDo: set bucketName dynamic
	var (
		bucketName       = "urchincache"
		dataMode         = form.DataMode
		fileHeader       = form.File
		datasetHashAlgo  = strings.Split(form.Digest, ":")[0]
		datasetHash      = strings.Split(form.Digest, ":")[1]
		datasetId        = form.DatasetId
		datasetVersionId = form.DatasetVersionId
	)

	// Handle task for backend.
	switch DataModeJson2Enum[dataMode] {
	case Meta:

		objectKey := urfm.genBackendMetaFileObjectKey(datasetId, datasetVersionId, form.Digest)

		client, err := urfm.client()
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		signURL, err := client.GetSignURL(ctx, bucketName, objectKey, objectstorage.MethodGet, defaultSignExpireTime)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		// Initialize url meta.
		urlMeta := &commonv1.UrlMeta{Filter: urfm.config.ObjectStorage.Filter}

		dgst := digest.New(datasetHashAlgo, datasetHash)
		urlMeta.Digest = ""

		// Initialize max replicas.
		maxReplicas := urfm.config.ObjectStorage.MaxReplicas

		// Initialize task id and peer id.
		taskID := idgen.TaskIDV1(signURL, urlMeta)
		peerID := urfm.peerIDGenerator.PeerID()

		log := logger.WithTaskAndPeerID(taskID, peerID)
		log.Infof("upload object %s meta: %s %#v", objectKey, signURL, urlMeta)

		// Import object to local storage.
		log.Infof("import object %s to local storage", objectKey)
		if err := urfm.importObjectToLocalStorage(ctx, taskID, peerID, fileHeader); err != nil {
			log.Error(err)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		// Announce peer information to scheduler.
		log.Info("announce peer to scheduler")
		if err := urfm.peerTaskManager.AnnouncePeerTask(ctx, storage.PeerTaskMetadata{
			TaskID: taskID,
			PeerID: peerID,
		}, signURL, commonv1.TaskType_DfStore, urlMeta); err != nil {
			log.Error(err)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		// Import object to seed peer.
		go func() {
			dataset, err := urchin_dataset.GetDataSetImpl(datasetId)
			if err != nil {
				return
			}
			if int(dataset.Replica) > maxReplicas {
				log.Errorf("replica %d is greater than max replicas %d", dataset.Replica, maxReplicas)
				return
			}

			if err := urfm.importObjectToSeedPeers(context.Background(), datasetId, datasetVersionId, bucketName, objectKey, urlMeta.Filter, form.Digest, ReplicaMetaOS, fileHeader, int(dataset.Replica), log); err != nil {
				log.Errorf("import object %s to seed peers failed: %s", objectKey, err)
			}
		}()

		// Import object to object storage.
		go func() {
			log.Infof("import object %s to bucket %s", objectKey, bucketName)
			if err := urfm.importObjectToBackend(context.Background(), urfm.config.ObjectStorage.Name, bucketName, objectKey, dgst, fileHeader, client); err != nil {
				log.Errorf("import object %s to bucket %s failed: %s", objectKey, bucketName, err.Error())
				return
			}
		}()

		ctx.Status(http.StatusOK)
		return
	case Chunk:
		fmt.Printf("!!!upload DataChunk, bucketName:%s, objectKey:%s !!!", bucketName)

		if err := urfm.storeLocalChunks(form.DatasetId, form.DatasetVersionId, form.Digest, form.ChunkStart, form.ChunkNum, fileHeader); err != nil {
			fmt.Errorf("storeDataChunk chunkStart:%d, ChunkNum:%d failed: %s", form.ChunkStart, form.ChunkNum, err.Error())
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		ctx.Status(http.StatusOK)
		return
	case ChunkEnd:
		fmt.Printf("!!!upload DataChunkEnd, bucketName:%s, objectKey:%s !!!", bucketName)

		if err := urfm.mergeLocalChunks(form.DatasetId, form.DatasetVersionId, form.Digest); err != nil {
			fmt.Errorf("storeDataChunk chunkStart:%d, ChunkNum:%d failed: %s", form.ChunkStart, form.ChunkNum, err.Error())
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		mergedDataBlobFilePath := urfm.genLocalBlobFilePath(datasetId, form.DatasetVersionId, form.Digest)

		backendBlobFileObjectKey := urfm.genBackendBlobFileObjectKey(datasetId, form.DatasetVersionId, form.Digest)

		client, err := urfm.client()
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		// Initialize url meta.
		urlMeta := &commonv1.UrlMeta{Filter: urfm.config.ObjectStorage.Filter}

		dgst := digest.New(datasetHashAlgo, datasetHash)

		// Initialize max replicas.
		maxReplicas := urfm.config.ObjectStorage.MaxReplicas

		// Initialize task id and peer id.
		taskID := dgst.String()
		peerID := urfm.peerIDGenerator.PeerID()

		log := logger.WithTaskAndPeerID(taskID, peerID)
		log.Infof("upload object %s meta: %s", backendBlobFileObjectKey, taskID)

		// Import file to local storage.
		log.Infof("import file %s to local storage", backendBlobFileObjectKey)
		if err := urfm.importFileToLocalStorage(ctx, taskID, peerID, mergedDataBlobFilePath); err != nil {
			log.Error(err)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		// Announce peer information to scheduler.
		log.Info("announce peer to scheduler")
		if err := urfm.peerTaskManager.AnnouncePeerTask(ctx, storage.PeerTaskMetadata{
			TaskID: taskID,
			PeerID: peerID,
		}, taskID, commonv1.TaskType_DfStore, urlMeta); err != nil {
			log.Error(err)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		importFileToSeedPeersSucceed := make(chan bool)
		importFileToBackendSucceed := make(chan bool)

		// Import local file to seed peer.
		go func() {
			log.Infof("import local blob file %s to seed-peer bucket %s", backendBlobFileObjectKey, bucketName)
			dataset, err := urchin_dataset.GetDataSetImpl(datasetId)
			if err != nil {
				return
			}
			if int(dataset.Replica) > maxReplicas {
				log.Errorf("replica %d is greater than max replicas %d", dataset.Replica, maxReplicas)
				return
			}

			if err := urfm.importFileToSeedPeers(context.Background(), datasetId, form.DatasetVersionId, bucketName, backendBlobFileObjectKey, urlMeta.Filter, form.Digest, ReplicaBlobOS, mergedDataBlobFilePath, int(dataset.Replica), log); err != nil {
				log.Errorf("import local blob file %s to seed peers failed: %s", backendBlobFileObjectKey, err)
				importFileToSeedPeersSucceed <- false
				return
			}

			importFileToSeedPeersSucceed <- true
		}()

		// Import local file to backend object storage.
		go func() {
			log.Infof("import local blob file %s to bucket %s", backendBlobFileObjectKey, bucketName)
			if err := urfm.importFileToBackend(context.Background(), bucketName, backendBlobFileObjectKey, dgst, mergedDataBlobFilePath, client); err != nil {
				if strings.Contains(err.Error(), "408 Request Timeout") {
					time.Sleep(time.Second * 3)
					log.Infof("put object %s failed, try again...", backendBlobFileObjectKey)
					if err := urfm.importFileToBackend(context.Background(), bucketName, backendBlobFileObjectKey, dgst, mergedDataBlobFilePath, client); err != nil {
						log.Errorf("import local blob file %s to bucket %s failed: %s", backendBlobFileObjectKey, bucketName, err.Error())
						importFileToBackendSucceed <- false
						return
					} else {
						importFileToBackendSucceed <- true
						return
					}
				}

				importFileToBackendSucceed <- false
				return
			}

			importFileToBackendSucceed <- true
		}()

		//Delete local blob file after importFileToSeedPeers and importFileToBackend
		go func() {
			//wait importFileToSeedPeers goroutine finish
			isSeedPeersOk := <-importFileToSeedPeersSucceed
			if !isSeedPeersOk {
				log.Errorf("import local blob file %s to seed peers failed, stop delete this dataset local files", mergedDataBlobFilePath)
				return
			}

			//wait importFileToBackend goroutine finish
			isBackendOk := <-importFileToBackendSucceed
			if !isBackendOk {
				log.Errorf("import local blob file %s to bucket %s failed, stop delete this dataset local files", mergedDataBlobFilePath, bucketName)
				return
			}

			log.Info("import blob file To seed peers & backend succeed! Go to delete this dataset local files to save disk space")

			if err := urfm.deleteLocalChunks(datasetId, datasetVersionId, dgst); err != nil {
				log.Errorf("deleteLocalChunks failed: %s, datasetId:%s, datasetVersionId:%s", err.Error(), datasetId, datasetVersionId)
				return
			}

			if err := urfm.deleteLocalBlobFile(datasetId, datasetVersionId, dgst); err != nil {
				log.Errorf("deleteLocalBlobFile failed: %s, datasetId:%s, datasetVersionId:%s", err.Error(), datasetId, datasetVersionId)
				return
			}
		}()

		ctx.JSON(http.StatusOK, gin.H{
			"content_length": fmt.Sprint(form.TotalSize),
			"digest":         form.Digest,
			"status_code":    0,
			"status_msg":     "Succeed",
			"task_id":        taskID,
		})
		return
	case ReplicaMetaOS, ReplicaBlobOS:
		var objectKey string
		if DataModeJson2Enum[dataMode] == ReplicaMetaOS {
			objectKey = urfm.genBackendMetaFileObjectKey(datasetId, datasetVersionId, form.Digest)
		} else {
			objectKey = urfm.genBackendBlobFileObjectKey(datasetId, datasetVersionId, form.Digest)
		}

		dgst := digest.New(datasetHashAlgo, datasetHash)
		logger.Infof("import replica object %s to bucket %s, digit:%s", objectKey, bucketName, dgst)

		dstClient, err := urfm.client()
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		if err := urfm.importObjectToBackend(ctx, urfm.config.ObjectStorage.Name, bucketName, objectKey, dgst, fileHeader, dstClient); err != nil {
			log.Error(err)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		ctx.JSON(http.StatusOK, gin.H{
			"status_code":   0,
			"status_msg":    "success",
			"endpoint":      urfm.config.ObjectStorage.Endpoint,
			"endpoint_path": bucketName + "." + objectKey,
		})
		return
	case Source, Ephemeral:
		fmt.Printf("!!!upload Source or DataCache, bucketName:%s, objectKey:%s !!!", bucketName)
		ctx.Status(http.StatusOK)
		return
	}

	ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": fmt.Sprintf("unknow dataMode %s", dataMode)})
	return
}

func (urfm *UrchinFileManager) StatFile(ctx *gin.Context) {
	var statParams StatFileParams
	if err := ctx.ShouldBindQuery(&statParams); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}
	statDataMode := statParams.DataMode
	switch DataModeJson2Enum[statDataMode] {
	case Meta:
		bucketName := "urchincache"
		backendMetaFileObjectKey := urfm.genBackendMetaFileObjectKey(statParams.DatasetId, statParams.DatasetVersionId, statParams.Digest)
		client, err := urfm.client()
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}
		_, isExist, err := client.GetObjectMetadata(ctx, bucketName, backendMetaFileObjectKey)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		if !isExist {
			backendFileNotFoundStat := urchinstatus.NewStatus(urchinstatus.NotFound)
			ctx.JSON(http.StatusOK, gin.H{
				"status_code":        backendFileNotFoundStat.StatusCode,
				"status_msg":         backendFileNotFoundStat.StatusMessage,
				"mode":               statParams.DataMode,
				"dataset_id":         statParams.DatasetId,
				"dataset_version_id": statParams.DatasetVersionId,
				"digest":             statParams.Digest,
			})
		} else {
			backendFileExistStat := urchinstatus.NewStatus(urchinstatus.Exist)
			ctx.JSON(http.StatusOK, gin.H{
				"status_code":        backendFileExistStat.StatusCode,
				"status_msg":         backendFileExistStat.StatusMessage,
				"mode":               statParams.DataMode,
				"dataset_id":         statParams.DatasetId,
				"dataset_version_id": statParams.DatasetVersionId,
				"digest":             statParams.Digest,
			})
		}

		return
	case Blob:
		//ToDo:get dataset source replicas filepath
		bucketName := "urchincache"
		backendBlobFileObjectKey := urfm.genBackendBlobFileObjectKey(statParams.DatasetId, statParams.DatasetVersionId, statParams.Digest)
		client, err := urfm.client()
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}
		_, isExist, err := client.GetObjectMetadata(ctx, bucketName, backendBlobFileObjectKey)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		if !isExist {
			isExist, err := urfm.checkLocalBlobFile(statParams)

			if err != nil {
				log.Error(err)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}

			if isExist {
				localFileExistStat := urchinstatus.NewStatus(urchinstatus.Exist)
				ctx.JSON(http.StatusOK, gin.H{
					"status_code":        localFileExistStat.StatusCode,
					"status_msg":         localFileExistStat.StatusMessage,
					"mode":               statParams.DataMode,
					"dataset_id":         statParams.DatasetId,
					"dataset_version_id": statParams.DatasetVersionId,
					"digest":             statParams.Digest,
				})
				return
			} else {
				localFileNotFoundStat := urchinstatus.NewStatus(urchinstatus.NotFound)
				ctx.JSON(http.StatusOK, gin.H{
					"status_code":        localFileNotFoundStat.StatusCode,
					"status_msg":         localFileNotFoundStat.StatusMessage,
					"mode":               statParams.DataMode,
					"dataset_id":         statParams.DatasetId,
					"dataset_version_id": statParams.DatasetVersionId,
					"digest":             statParams.Digest,
				})
				return
			}
		}

		backendFileExistStat := urchinstatus.NewStatus(urchinstatus.Exist)
		ctx.JSON(http.StatusOK, gin.H{
			"status_code":        backendFileExistStat.StatusCode,
			"status_msg":         backendFileExistStat.StatusMessage,
			"mode":               statParams.DataMode,
			"dataset_id":         statParams.DatasetId,
			"dataset_version_id": statParams.DatasetVersionId,
			"digest":             statParams.Digest,
		})
		return
	case Chunk:
		isExist, err := urfm.checkLocalChunkFile(statParams)
		if err != nil {
			log.Error(err)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}
		if isExist {
			localFileExistStat := urchinstatus.NewStatus(urchinstatus.Exist)
			ctx.JSON(http.StatusOK, gin.H{
				"status_code":        localFileExistStat.StatusCode,
				"status_msg":         localFileExistStat.StatusMessage,
				"mode":               statParams.DataMode,
				"dataset_id":         statParams.DatasetId,
				"dataset_version_id": statParams.DatasetVersionId,
				"digest":             statParams.Digest,
				"chunk_num":          statParams.ChunkNum,
				"chunk_start":        statParams.ChunkStart,
				"chunk_size":         statParams.ChunkSize,
			})
			return
		} else {
			localFileNotFoundStat := urchinstatus.NewStatus(urchinstatus.NotFound)
			ctx.JSON(http.StatusOK, gin.H{
				"status_code":        localFileNotFoundStat.StatusCode,
				"status_msg":         localFileNotFoundStat.StatusMessage,
				"mode":               statParams.DataMode,
				"dataset_id":         statParams.DatasetId,
				"dataset_version_id": statParams.DatasetVersionId,
				"digest":             statParams.Digest,
				"chunk_num":          statParams.ChunkNum,
				"chunk_start":        statParams.ChunkStart,
				"chunk_size":         statParams.ChunkSize,
			})
			return
		}
	}

	ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": fmt.Sprintf("unknow dataMode %d", statDataMode)})
	return
}

func (urfm *UrchinFileManager) checkLocalBlobFile(statParams StatFileParams) (bool, error) {
	tryLocalBlobFilePath := urfm.genLocalBlobFilePath(statParams.DatasetId, statParams.DatasetVersionId, statParams.Digest)
	localBlobFileInfo, err := os.Stat(tryLocalBlobFilePath)
	if err == nil {
		logger.Infof("check local blob file %q exists", tryLocalBlobFilePath)
		isPass := urfm.validateLocalBlobFile(uint64(localBlobFileInfo.Size()), statParams.TotalSize)
		return isPass, nil
	} else if !os.IsExist(err) {
		return false, nil
	} else {
		return false, err
	}
}

func (urfm *UrchinFileManager) validateLocalBlobFile(localFileSize uint64, expectFileSize uint64) bool {
	return localFileSize == expectFileSize
}

// deleteLocalBlobFile to save local disk space
func (urfm *UrchinFileManager) deleteLocalBlobFile(datasetId string, versionId string, versionDigest *digest.Digest) error {
	tryLocalBlobFilePath := urfm.genLocalBlobFilePath(datasetId, versionId, versionDigest.String())

	_, err := os.Stat(tryLocalBlobFilePath)
	if err == nil {
		logger.Infof("Blob File %q exists, delete it", tryLocalBlobFilePath)
		os.Remove(tryLocalBlobFilePath)
	}

	return err
}

func (urfm *UrchinFileManager) checkLocalChunkFile(statParams StatFileParams) (bool, error) {
	localChunksParentDir := urfm.genLocalChunksParentDir(statParams.DatasetId, statParams.DatasetVersionId, statParams.Digest)
	tryLocalChunkFilePath := urfm.genLocalChunkFilePath(localChunksParentDir, statParams.ChunkNum, statParams.ChunkStart)
	localChunkFileInfo, err := os.Stat(tryLocalChunkFilePath)
	if err == nil {
		logger.Infof("check local chunk file %q exists", tryLocalChunkFilePath)
		isPass := urfm.validateLocalChunkFile(uint64(localChunkFileInfo.Size()), statParams)
		return isPass, nil
	} else if !os.IsExist(err) {
		return false, nil
	} else {
		return false, err
	}
}

// ToDo: validate chunk file blob file digest
func (urfm *UrchinFileManager) validateLocalChunkFile(localFileSize uint64, statParams StatFileParams) bool {
	chunkEnd := statParams.ChunkStart + statParams.ChunkSize
	return localFileSize == statParams.ChunkSize && chunkEnd >= 0 && chunkEnd <= statParams.TotalSize
}

// deleteLocalChunks to save local disk space
func (urfm *UrchinFileManager) deleteLocalChunks(datasetId string, versionId string, versionDigest *digest.Digest) error {
	localChunksParentDir := urfm.genLocalChunksParentDir(datasetId, versionId, versionDigest.String())
	_, err := os.Stat(localChunksParentDir)
	if err == nil {
		logger.Infof("Chunks Dir %q exists, delete it", localChunksParentDir)
		os.RemoveAll(localChunksParentDir)
	}
	return err
}

func (urfm *UrchinFileManager) storeLocalChunks(datasetId string, versionId string, versionDigest string, chunkStart uint64, chunkNum uint64, uploadDataChunk *multipart.FileHeader) error {
	uploadChunkFile, err := uploadDataChunk.Open()
	if err != nil {
		return err
	}
	defer uploadChunkFile.Close()
	chunksParentDir := urfm.genLocalChunksParentDir(datasetId, versionId, versionDigest)
	if err := os.MkdirAll(chunksParentDir, defaultDirectoryMode); err != nil && !os.IsExist(err) {
		return err
	}
	chunkFilePath := urfm.genLocalChunkFilePath(chunksParentDir, chunkNum, chunkStart)
	_, err = os.Stat(chunkFilePath)
	if err == nil {
		logger.Infof("destination chunk file %q exists, purge it first", chunkFilePath)
		os.Remove(chunkFilePath)
	}
	localChunkFile, err := os.OpenFile(chunkFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, defaultFileMode)
	if err != nil {
		logger.Errorf("open destination chunk file error: %s", err)
		return err
	}
	defer localChunkFile.Close()
	n, err := io.Copy(localChunkFile, uploadChunkFile)
	logger.Debugf("copied chunk file data %d bytes to %s", n, chunkFilePath)
	return err
}

func (urfm *UrchinFileManager) mergeLocalChunks(datasetId string, versionId string, versionDigest string) error {
	localBlobFileParentDir := urfm.genLocalBlobFileParentDir(datasetId)
	if err := os.MkdirAll(localBlobFileParentDir, defaultDirectoryMode); err != nil && !os.IsExist(err) {
		return err
	}
	localBlobFilePath := urfm.genLocalBlobFilePath(datasetId, versionId, versionDigest)
	_, err := os.Stat(localBlobFilePath)
	if err == nil {
		// remove exist file
		logger.Infof("destination datasetBlobFile %q exists, purge it first", localBlobFilePath)
		os.Remove(localBlobFilePath)
	}
	localBlobFile, err := os.OpenFile(localBlobFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, defaultFileMode)
	if err != nil {
		return err
	}
	defer localBlobFile.Close()
	localChunksParentDir := urfm.genLocalChunksParentDir(datasetId, versionId, versionDigest)
	if err := os.MkdirAll(localChunksParentDir, defaultDirectoryMode); err != nil && !os.IsExist(err) {
		return err
	}
	localChunkFileInfos, err := os.ReadDir(localChunksParentDir)
	if err != nil {
		return err
	}

	sort.Slice(localChunkFileInfos, func(pro, pre int) bool {
		fileNamePre := localChunkFileInfos[pre].Name()

		chunkNumPre, _ := strconv.ParseUint(strings.Split(fileNamePre, "-")[0], 10, 64)

		fileNamePro := localChunkFileInfos[pro].Name()

		chunkNumPro, _ := strconv.ParseUint(strings.Split(fileNamePro, "-")[0], 10, 64)
		//True will change item position
		return chunkNumPre > chunkNumPro
	})

	for _, localChunkFileInfo := range localChunkFileInfos {
		localChunkFileName := localChunkFileInfo.Name()

		localChunkFilePath := path.Join(localChunksParentDir, localChunkFileName)

		localChunkFile, err := os.OpenFile(localChunkFilePath, os.O_RDONLY, defaultFileMode)
		if err != nil {
			return err
		}

		_, err = io.Copy(localBlobFile, localChunkFile)

		if err != nil {
			return err
		}

		logger.Infof("merged chunkFile Name:%s", localChunkFileName)

		localChunkFile.Close()
	}

	return nil
}

// importFileToBackend uses to import file to backend.
func (urfm *UrchinFileManager) importFileToBackend(ctx context.Context, bucketName, objectKey string, dgst *digest.Digest, sourceFilePath string, client objectstorage.ObjectStorage) error {
	sourceFile, err := os.OpenFile(sourceFilePath, os.O_RDONLY, defaultFileMode)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	if err := client.PutObject(ctx, bucketName, objectKey, dgst.String(), sourceFile); err != nil {
		return err
	}
	return nil
}

// importFileToLocalStorage uses to import file to local storage.
func (urfm *UrchinFileManager) importFileToLocalStorage(ctx context.Context, taskID, peerID string, sourceFilePath string) error {

	sourceFile, err := os.OpenFile(sourceFilePath, os.O_RDONLY, defaultFileMode)
	if err != nil {
		return err
	}
	sourceFileInfo, err := sourceFile.Stat()
	if err != nil {
		return err
	}

	defer sourceFile.Close()

	meta := storage.PeerTaskMetadata{
		TaskID: taskID,
		PeerID: peerID,
	}

	// Register task.
	tsd, err := urfm.storageManager.RegisterTask(ctx, &storage.RegisterTaskRequest{
		PeerTaskMetadata: meta,
	})
	if err != nil {
		return err
	}

	// Import task data to dfdaemon.
	if err := urfm.peerTaskManager.GetPieceManager().Import(ctx, meta, tsd, sourceFileInfo.Size(), sourceFile); err != nil {
		return err
	}

	return nil
}

// importFileToSeedPeers uses to import file to available seed peers.
func (urfm *UrchinFileManager) importFileToSeedPeers(ctx context.Context, datasetId, datasetVersionId, bucketName, objectKey, filter, digest string, mode int, sourceFilePath string, maxReplicas int, log *logger.SugaredLoggerOnWith) error {
	seedPeerHosts, err := urfm.getSeedPeerHosts(datasetId, mode)
	if err != nil {
		log.Errorf("importFileToSeedPeers get seed peer hosts failed: %s", err)
		return err
	}

	if maxReplicas > len(seedPeerHosts) {
		log.Errorf("replica %d is greater than max replicas %d", maxReplicas, len(seedPeerHosts))
		return fmt.Errorf("replica %d is greater than max replicas %d", maxReplicas, len(seedPeerHosts))
	}

	var replicas int
	var urchinDataCache []urchin_dataset.UrchinEndpoint
	for _, seedPeerHost := range seedPeerHosts {
		log.Infof("import file %s to seed peer %s", objectKey, seedPeerHost)
		if mode == ReplicaBlobOS {
			urchinEndpoint, err := urfm.importFileToSeedPeerWithResult(ctx, datasetId, datasetVersionId, seedPeerHost, mode, digest, sourceFilePath)
			if err != nil {
				log.Errorf("import file %s to seed peer %s failed: %s", objectKey, seedPeerHost, err)
				continue
			}
			urchinDataCache = append(urchinDataCache, *urchinEndpoint)
		} else {
			if err := urfm.importFileToSeedPeer(ctx, seedPeerHost, bucketName, objectKey, filter, mode, sourceFilePath); err != nil {
				log.Errorf("import file %s to seed peer %s failed: %s", objectKey, seedPeerHost, err)
				continue
			}
		}

		replicas++
		if replicas >= maxReplicas {
			break
		}
	}

	if mode == ReplicaBlobOS {
		configStorage, err := urfm.dynconfig.GetObjectStorage()
		if err != nil {
			return err
		}

		urchinDataSource := []urchin_dataset.UrchinEndpoint{
			{
				Endpoint:     configStorage.Endpoint,
				EndpointPath: bucketName + "." + objectKey,
			},
		}

		err = urchin_dataset.UpdateDataSetImpl(datasetId, "", "", 0, "", []string{}, urchinDataSource, urchinDataCache)
		if err != nil {
			log.Errorf("UpdateDataSetImpl file %s to bucket %s failed, error %s", objectKey, bucketName, err)
			return err
		}
	}

	if replicas < maxReplicas {
		log.Errorf("import replica num %d file %s to seed peers some failed", replicas, objectKey)
		return fmt.Errorf("import replica num %d file %s to seed peers some failed", replicas, objectKey)
	}

	log.Infof("import %d file %s to seed peers", replicas, objectKey)
	return nil
}

// importFileToSeedPeer uses to import object to seed peer.
func (urfm *UrchinFileManager) importFileToSeedPeer(ctx context.Context, seedPeerHost, bucketName, objectKey, filter string, mode int, sourceFilePath string) error {
	sourceFile, err := os.OpenFile(sourceFilePath, os.O_RDONLY, defaultFileMode)
	if err != nil {
		return err
	}
	sourceFileInfo, err := sourceFile.Stat()
	if err != nil {
		return err
	}
	sourceFileName := sourceFileInfo.Name()
	defer sourceFile.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if err := writer.WriteField("mode", fmt.Sprint(mode)); err != nil {
		return err
	}

	if filter != "" {
		if err := writer.WriteField("filter", filter); err != nil {
			return err
		}
	}

	part, err := writer.CreateFormFile("file", sourceFileName)
	if err != nil {
		return err
	}

	if _, err := io.Copy(part, sourceFile); err != nil {
		return err
	}

	if err := writer.Close(); err != nil {
		return err
	}

	u := url.URL{
		Scheme: "http",
		Host:   seedPeerHost,
		Path:   filepath.Join("buckets", bucketName, "objects", objectKey),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), body)
	if err != nil {
		return err
	}
	req.Header.Add(headers.ContentType, writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("bad response status %s", resp.Status)
	}

	return nil
}

// importFileToSeedPeer uses to import object to seed peer.
func (urfm *UrchinFileManager) importFileToSeedPeerWithResult(ctx context.Context, datasetId, datasetVersionId, seedPeerHost string, mode int, digest, sourceFilePath string) (*urchin_dataset.UrchinEndpoint, error) {
	sourceFile, err := os.OpenFile(sourceFilePath, os.O_RDONLY, defaultFileMode)
	if err != nil {
		return nil, err
	}
	sourceFileInfo, err := sourceFile.Stat()
	if err != nil {
		return nil, err
	}
	sourceFileName := sourceFileInfo.Name()
	defer sourceFile.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if err := writer.WriteField("mode", DataModeEnum2Json[int32(mode)]); err != nil {
		return nil, err
	}

	if err := writer.WriteField("digest", digest); err != nil {
		return nil, err
	}

	if err := writer.WriteField("dataset_id", datasetId); err != nil {
		return nil, err
	}

	if err := writer.WriteField("dataset_version_id", datasetVersionId); err != nil {
		return nil, err
	}

	if err := writer.WriteField("total_size", strconv.FormatInt(sourceFileInfo.Size(), 10)); err != nil {
		return nil, err
	}

	part, err := writer.CreateFormFile("file", sourceFileName)
	if err != nil {
		return nil, err
	}

	if _, err := io.Copy(part, sourceFile); err != nil {
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	u := url.URL{
		Scheme: "http",
		Host:   seedPeerHost,
		Path:   filepath.Join("api/v1/file/upload"),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Add(headers.ContentType, writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("bad response status %s", resp.Status)
	}

	respBody, _ := io.ReadAll(resp.Body)
	var result map[string]any
	err = json.Unmarshal(respBody, &result)
	if err != nil {
		return nil, err
	}

	if int(result["status_code"].(float64)) != 0 {
		return nil, fmt.Errorf("bad response status %v", result["status_code"])
	}

	urchinEndpoint := urchin_dataset.UrchinEndpoint{
		Endpoint:     result["endpoint"].(string),
		EndpointPath: result["endpoint_path"].(string),
	}

	return &urchinEndpoint, nil
}

// importObjectToBackend uses to import object to backend.
func (urfm *UrchinFileManager) importObjectToBackend(ctx context.Context, storageName, bucketName, objectKey string, dgst *digest.Digest, fileHeader *multipart.FileHeader, client objectstorage.ObjectStorage) error {
	f, err := fileHeader.Open()
	if err != nil {
		return err
	}
	defer f.Close()

	if storageName == "sugon" || storageName == "starlight" {
		if err := client.PutObjectWithTotalLength(ctx, bucketName, objectKey, dgst.String(), fileHeader.Size, f); err != nil {
			return err
		}
	} else {
		if err := client.PutObject(ctx, bucketName, objectKey, dgst.String(), f); err != nil {
			return err
		}
	}

	return nil
}

// importObjectToSeedPeers uses to import object to local storage.
func (urfm *UrchinFileManager) importObjectToLocalStorage(ctx context.Context, taskID, peerID string, fileHeader *multipart.FileHeader) error {
	f, err := fileHeader.Open()
	if err != nil {
		return nil
	}
	defer f.Close()

	meta := storage.PeerTaskMetadata{
		TaskID: taskID,
		PeerID: peerID,
	}

	// Register task.
	tsd, err := urfm.storageManager.RegisterTask(ctx, &storage.RegisterTaskRequest{
		PeerTaskMetadata: meta,
	})
	if err != nil {
		return err
	}

	// Import task data to dfdaemon.
	if err := urfm.peerTaskManager.GetPieceManager().Import(ctx, meta, tsd, fileHeader.Size, f); err != nil {
		return err
	}

	return nil
}

func (urfm *UrchinFileManager) getAllSeedPeer() ([]string, error) {
	var seedPeerHosts []string
	schedulers, err := urfm.dynconfig.GetSchedulers()
	if err != nil {
		return nil, err
	}

	for _, scheduler := range schedulers {
		for _, seedPeer := range scheduler.SeedPeers {
			if urfm.config.Host.AdvertiseIP.String() != seedPeer.Ip && seedPeer.ObjectStoragePort > 0 {
				seedPeerHosts = append(seedPeerHosts, fmt.Sprintf("%s:%d", seedPeer.Ip, seedPeer.ObjectStoragePort))
			}
		}
	}
	seedPeerHosts = pkgstrings.Unique(seedPeerHosts)

	rand.Seed(time.Now().Unix())
	rand.Shuffle(len(seedPeerHosts), func(i, j int) {
		seedPeerHosts[i], seedPeerHosts[j] = seedPeerHosts[j], seedPeerHosts[i]
	})

	return seedPeerHosts, nil
}

func (urfm *UrchinFileManager) makeSeedPeerHosts(datasetId string, maxReplicas, mode int) ([]string, error) {
	if mode != ReplicaMetaOS && mode != ReplicaBlobOS {
		return urfm.getAllSeedPeer()
	}

	redisClient := util.NewRedisStorage(util.RedisClusterIP, util.RedisClusterPwd, false)
	replicaKey := redisClient.MakeStorageKey([]string{"replica", "seed-peer", datasetId}, "")
	exists, err := redisClient.Exists(replicaKey)
	if err != nil {
		return nil, err
	}
	if exists {
		value, err := redisClient.Get(replicaKey)
		if err != nil {
			return nil, err
		}

		var seedPeerHosts []string
		err = json.Unmarshal(value, &seedPeerHosts)
		if err != nil {
			return nil, err
		}

		return seedPeerHosts, nil
	}

	seedPeerHosts, err := urfm.getAllSeedPeer()
	if err != nil {
		return nil, err
	}
	if len(seedPeerHosts) < maxReplicas {
		maxReplicas = len(seedPeerHosts)
	}

	seedPeerHosts = seedPeerHosts[0:maxReplicas]
	jsonBody, err := json.Marshal(seedPeerHosts)
	if err != nil {
		return nil, err
	}

	err = redisClient.Set(replicaKey, jsonBody)
	if err != nil {
		return nil, err
	}

	return seedPeerHosts, nil
}

func (urfm *UrchinFileManager) getSeedPeerHosts(datasetId string, mode int) ([]string, error) {
	if mode != ReplicaMetaOS && mode != ReplicaBlobOS {
		return urfm.getAllSeedPeer()
	}

	redisClient := util.NewRedisStorage(util.RedisClusterIP, util.RedisClusterPwd, false)
	replicaKey := redisClient.MakeStorageKey([]string{"replica", "seed-peer", datasetId}, "")

	value, err := redisClient.Get(replicaKey)
	if err != nil {
		return nil, err
	}

	var seedPeerHosts []string
	err = json.Unmarshal(value, &seedPeerHosts)
	if err != nil {
		return nil, err
	}

	return seedPeerHosts, nil
}

// importObjectToSeedPeers uses to import object to available seed peers.
func (urfm *UrchinFileManager) importObjectToSeedPeers(ctx context.Context, datasetId, datasetVersionId, bucketName, objectKey, filter, digest string, mode int, fileHeader *multipart.FileHeader, maxReplicas int, log *logger.SugaredLoggerOnWith) error {
	seedPeerHosts, err := urfm.makeSeedPeerHosts(datasetId, maxReplicas, mode)
	if err != nil {
		return err
	}

	if maxReplicas > len(seedPeerHosts) {
		log.Errorf("replica %d is greater than max replicas %d", maxReplicas, len(seedPeerHosts))
		return fmt.Errorf("replica %d is greater than max replicas %d", maxReplicas, len(seedPeerHosts))
	}

	var replicas int
	var urchinMetaCache []urchin_dataset.UrchinEndpoint
	for _, seedPeerHost := range seedPeerHosts {
		log.Infof("import object %s to seed peer %s", objectKey, seedPeerHost)

		if mode == ReplicaMetaOS {
			urchinEndpoint, err := urfm.importObjectToSeedPeerWithResult(ctx, datasetId, datasetVersionId, seedPeerHost, mode, digest, fileHeader)
			if err != nil {
				log.Errorf("import object %s to seed peer %s failed: %s", objectKey, seedPeerHost, err)
				continue
			}
			urchinMetaCache = append(urchinMetaCache, *urchinEndpoint)
		} else {
			if err := urfm.importObjectToSeedPeer(ctx, seedPeerHost, bucketName, objectKey, filter, mode, fileHeader); err != nil {
				log.Errorf("import object %s to seed peer %s failed: %s", objectKey, seedPeerHost, err)
				continue
			}
		}

		replicas++
		if replicas >= maxReplicas {
			break
		}
	}

	if mode == ReplicaMetaOS {
		configStorage, err := urfm.dynconfig.GetObjectStorage()
		if err != nil {
			return err
		}

		urchinMetaSource := []urchin_dataset.UrchinEndpoint{
			{
				Endpoint:     configStorage.Endpoint,
				EndpointPath: bucketName + "." + objectKey,
			},
		}

		metaSource, _ := json.Marshal(urchinMetaSource)
		metaCache, _ := json.Marshal(urchinMetaCache)

		UrchinDataSetVersionInfo := urchin_dataset_vesion.UrchinDataSetVersionInfo{
			MetaSources: string(metaSource),
			MetaCaches:  string(metaCache),
		}
		err = urchin_dataset_vesion.UpdateDataSetVersionImpl(datasetId, datasetVersionId, UrchinDataSetVersionInfo)
		if err != nil {
			log.Errorf("UpdateDataSetVersionImpl file %s to bucket %s failed, error %s", objectKey, bucketName, err)
			return err
		}
	}

	if replicas < maxReplicas {
		log.Errorf("import replica num %d object %s to seed peers some failed", replicas, objectKey)
		return fmt.Errorf("import replica num %d object %s to seed peers some failed", replicas, objectKey)
	}

	log.Infof("import %d object %s to seed peers", replicas, objectKey)
	return nil
}

// importObjectToSeedPeer uses to import object to seed peer.
func (urfm *UrchinFileManager) importObjectToSeedPeer(ctx context.Context, seedPeerHost, bucketName, objectKey, filter string, mode int, fileHeader *multipart.FileHeader) error {
	f, err := fileHeader.Open()
	if err != nil {
		return err
	}
	defer f.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if err := writer.WriteField("mode", fmt.Sprint(mode)); err != nil {
		return err
	}

	if filter != "" {
		if err := writer.WriteField("filter", filter); err != nil {
			return err
		}
	}

	part, err := writer.CreateFormFile("file", fileHeader.Filename)
	if err != nil {
		return err
	}

	if _, err := io.Copy(part, f); err != nil {
		return err
	}

	if err := writer.Close(); err != nil {
		return err
	}

	u := url.URL{
		Scheme: "http",
		Host:   seedPeerHost,
		Path:   filepath.Join("buckets", bucketName, "objects", objectKey),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), body)
	if err != nil {
		return err
	}
	req.Header.Add(headers.ContentType, writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("bad response status %s", resp.Status)
	}

	return nil
}

// importObjectToSeedPeer uses to import object to seed peer.
func (urfm *UrchinFileManager) importObjectToSeedPeerWithResult(ctx context.Context, datasetId, datasetVersionId, seedPeerHost string, mode int, digest string, fileHeader *multipart.FileHeader) (*urchin_dataset.UrchinEndpoint, error) {
	f, err := fileHeader.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if err := writer.WriteField("mode", DataModeEnum2Json[int32(mode)]); err != nil {
		return nil, err
	}

	if err := writer.WriteField("digest", digest); err != nil {
		return nil, err
	}

	if err := writer.WriteField("dataset_id", datasetId); err != nil {
		return nil, err
	}

	if err := writer.WriteField("dataset_version_id", datasetVersionId); err != nil {
		return nil, err
	}

	if err := writer.WriteField("total_size", strconv.FormatInt(fileHeader.Size, 10)); err != nil {
		return nil, err
	}

	part, err := writer.CreateFormFile("file", fileHeader.Filename)
	if err != nil {
		return nil, err
	}

	if _, err := io.Copy(part, f); err != nil {
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	u := url.URL{
		Scheme: "http",
		Host:   seedPeerHost,
		Path:   filepath.Join("api/v1/file/upload"),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Add(headers.ContentType, writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("bad response status %s", resp.Status)
	}

	respBody, _ := io.ReadAll(resp.Body)
	var result map[string]any
	err = json.Unmarshal(respBody, &result)
	if err != nil {
		return nil, err
	}

	if int(result["status_code"].(float64)) != 0 {
		return nil, fmt.Errorf("bad response status %v", result["status_code"])
	}

	urchinEndpoint := urchin_dataset.UrchinEndpoint{
		Endpoint:     result["endpoint"].(string),
		EndpointPath: result["endpoint_path"].(string),
	}

	return &urchinEndpoint, nil
}

// client uses to generate client of object storage.
func (urfm *UrchinFileManager) client() (objectstorage.ObjectStorage, error) {
	//config, err := urfm.dynconfig.GetObjectStorage()
	//if err != nil {
	//	return nil, err
	//}

	client, err := objectstorage.New(urfm.config.ObjectStorage.Name, urfm.config.ObjectStorage.Region, urfm.config.ObjectStorage.Endpoint,
		urfm.config.ObjectStorage.AccessKey, urfm.config.ObjectStorage.SecretKey)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func (urfm *UrchinFileManager) CacheObject(ctx *gin.Context) {
	var params objectstorage.ObjectParams
	if err := ctx.ShouldBindUri(&params); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var query objectstorage.GetObjectQuery
	if err := ctx.ShouldBindQuery(&query); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	bucketEndpoint := strings.SplitN(params.ID, ".", 2)

	if len(bucketEndpoint) < 2 {
		logger.Errorf("sourceBucket %s is invalid", bucketEndpoint)
		ctx.JSON(http.StatusNotFound, gin.H{"errors": http.StatusText(http.StatusNotFound)})
		return
	}

	var (
		sourceEndpoint = bucketEndpoint[1]
		bucketName     = bucketEndpoint[0]
		objectKey      = strings.TrimPrefix(params.ObjectKey, string(os.PathSeparator))
		filter         = query.Filter
		rg             *nethttp.Range
		err            error
	)

	//1. Check Endpoint
	isInControl := objectstorage.ConfirmDataSourceInBackendPool(urfm.config, sourceEndpoint)
	if !isInControl {
		ctx.JSON(http.StatusForbidden, gin.H{"errors": "DataSource Not In BackendPool:" + http.StatusText(http.StatusForbidden)})
		return
	}
	//2. Check Target Bucket
	if !objectstorage.CheckTargetBucketIsInControl(urfm.config, sourceEndpoint, bucketName) {
		ctx.JSON(http.StatusForbidden, gin.H{"errors": "please check datasource bucket & cache bucket is in control & exist in config"})
		return
	}

	// Initialize filter field.
	urlMeta := &commonv1.UrlMeta{Filter: urfm.config.ObjectStorage.Filter}
	if filter != "" {
		urlMeta.Filter = filter
	}

	//3. Check Current Peer CacheBucket is existed datasource Object
	dstClient, err := objectstorage.Client(urfm.config.ObjectStorage.Name,
		urfm.config.ObjectStorage.Region,
		urfm.config.ObjectStorage.Endpoint,
		urfm.config.ObjectStorage.AccessKey,
		urfm.config.ObjectStorage.SecretKey)

	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	anyRes, _, err := retry.Run(ctx, 0.05, 0.2, 3, func() (any, bool, error) {
		ctxSub, cancel := context.WithTimeout(ctx, time.Duration(urfm.config.ObjectStorage.RetryTimeOutSec)*time.Second)
		defer cancel()

		meta, isExist, err := dstClient.GetObjectMetadata(ctxSub, urfm.config.ObjectStorage.CacheBucket, objectKey)
		if isExist && err == nil {
			//exist, skip retry
			return objectstorage.RetryRes{Res: meta, IsExist: isExist}, true, nil
		} else if err != nil && objectstorage.NeedRetry(err) {
			//retry this request, do not cancel this request
			return objectstorage.RetryRes{Res: nil, IsExist: false}, false, err
		} else {
			//retry this request, do not cancel this request
			return objectstorage.RetryRes{Res: nil, IsExist: false}, true, err
		}
	})

	if err != nil {
		logger.Errorf("Endpoint %s CacheBucket %s get meta error:%s", urfm.config.ObjectStorage.Endpoint, urfm.config.ObjectStorage.CacheBucket, err.Error())
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	dataRootName := urfm.config.ObjectStorage.CacheBucket
	if urfm.config.ObjectStorage.Name == "sugon" || urfm.config.ObjectStorage.Name == "starlight" {
		dataRootName = filepath.Join("/", strings.ReplaceAll(dataRootName, "-", "/"))
	}

	res := anyRes.(objectstorage.RetryRes)
	if res.IsExist {
		meta := res.Res
		ctx.JSON(http.StatusOK, gin.H{
			headers.ContentType:                 meta.ContentType,
			headers.ContentLength:               fmt.Sprint(meta.ContentLength),
			config.HeaderUrchinObjectMetaDigest: meta.Digest,
			"StatusCode":                        0,
			"StatusMsg":                         "Succeed",
			"TaskID":                            "",
			"SignedUrl":                         "",
			"DataRoot":                          dataRootName,
			"DataPath":                          objectKey,
			"DataEndpoint":                      urfm.config.ObjectStorage.Endpoint,
		})

		return
	} else {
		//4.Check Exist in DstDataCenter's Control Buckets
		for _, bucket := range urfm.config.ObjectStorage.Buckets {
			if bucket.Name != urfm.config.ObjectStorage.CacheBucket && bucket.Enable {

				meta, isExist, err := dstClient.GetObjectMetadata(ctx, bucket.Name, objectKey)
				dataRootName := bucket.Name
				if urfm.config.ObjectStorage.Name == "sugon" || urfm.config.ObjectStorage.Name == "starlight" {
					dataRootName = filepath.Join("/", strings.ReplaceAll(dataRootName, "-", "/"))
				}

				if isExist {

					ctx.JSON(http.StatusOK, gin.H{
						headers.ContentType:                 meta.ContentType,
						headers.ContentLength:               fmt.Sprint(meta.ContentLength),
						config.HeaderUrchinObjectMetaDigest: meta.Digest,
						"StatusCode":                        0,
						"StatusMsg":                         "Succeed",
						"TaskID":                            "",
						"SignedUrl":                         "",
						"DataRoot":                          dataRootName,
						"DataPath":                          objectKey,
						"DataEndpoint":                      urfm.config.ObjectStorage.Endpoint,
					})

					return
				}

				if err != nil {
					logger.Errorf("Endpoint %s Control Bucket %s get meta error:%s", urfm.config.ObjectStorage.Endpoint, bucket.Name, err.Error())
					ctx.JSON(http.StatusInternalServerError, gin.H{"errors": "Endpoint " + urfm.config.ObjectStorage.Endpoint + " Control Bucket " + bucket.Name + " get meta error:" + err.Error()})
					return
				}
			}
		}

		logger.Infof("DstData Endpoint %s Control Buckets not exist object:%s", urfm.config.ObjectStorage.Endpoint, objectKey)
	}

	//DstDataSet Not Exist In DstDataCenter, Move it from DataSource to Dst DataCenter
	//5. Check dataSource is existed this Object
	sourceClient, err := objectstorage.Client(urfm.config.SourceObs.Name,
		urfm.config.SourceObs.Region,
		urfm.config.SourceObs.Endpoint,
		urfm.config.SourceObs.AccessKey,
		urfm.config.SourceObs.SecretKey)

	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	anyRes, _, err = retry.Run(ctx, 0.05, 0.2, 3, func() (any, bool, error) {
		//ctxSub, cancel := context.WithTimeout(ctx, 3*time.Second)
		//defer cancel()

		meta, isExist, err := sourceClient.GetObjectMetadata(ctx, bucketName, objectKey)
		if isExist && err == nil {
			//exist, skip retry
			return objectstorage.RetryRes{Res: meta, IsExist: isExist}, true, nil
		} else if err != nil && objectstorage.NeedRetry(err) {
			//retry this request, do not cancel this request
			return objectstorage.RetryRes{Res: nil, IsExist: false}, false, err
		} else {
			return objectstorage.RetryRes{Res: nil, IsExist: false}, true, err
		}
	})

	if err != nil {
		logger.Errorf("dataSource %s bucket %s object %s err %v ", urfm.config.SourceObs.Endpoint, bucketName, objectKey, err)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}
	res = anyRes.(objectstorage.RetryRes)
	if !res.IsExist {
		logger.Errorf("dataSource %s bucket %s object %s not found", urfm.config.SourceObs.Endpoint, bucketName, objectKey)
		ctx.JSON(http.StatusNotFound, gin.H{"errors": "dataSource:" + urfm.config.SourceObs.Endpoint + "." + bucketName + "/" + objectKey + " not found"})
		return
	}

	meta := res.Res

	urlMeta.Digest = meta.Digest

	// Parse http range header.
	rangeHeader := ctx.GetHeader(headers.Range)
	if len(rangeHeader) > 0 {
		rangeValue, err := nethttp.ParseOneRange(rangeHeader, math.MaxInt64)
		if err != nil {
			ctx.JSON(http.StatusRequestedRangeNotSatisfiable, gin.H{"errors": err.Error()})
			return
		}
		rg = &rangeValue

		// Range header in dragonfly is without "bytes=".
		urlMeta.Range = strings.TrimLeft(rangeHeader, "bytes=")
	}

	signURL, err := sourceClient.GetSignURL(ctx, bucketName, objectKey, objectstorage.MethodGet, defaultSignExpireTime)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	signURL, urlMeta = objectstorage.ConvertSignURL(ctx, signURL, urlMeta)

	taskID := idgen.TaskIDV1(signURL, urlMeta)
	log := logger.WithTaskID(taskID)

	urfm.cachingTaskLock.Lock()
	_, ok := urfm.cachingTasks.Load(taskID)

	log.Infof("cacheObject object %s taskID %s isCaching:%v meta: %s %#v", objectKey, taskID, ok, signURL, urlMeta)

	if !ok {
		urfm.cachingTasks.Store(taskID, true)

		go func() {

			reader, _, _, _, err := urfm.peerTaskManager.StartStreamTask(ctx, &peer.StreamTaskRequest{
				URL:     signURL,
				URLMeta: urlMeta,
				Range:   rg,
				PeerID:  urfm.peerIDGenerator.PeerID(),
			})

			if err != nil {

				logger.Errorf("StartStreamTask dstEndpoint %s object %s to sourceBucket %s taskID:%s err: %s and delete caching task", urfm.config.ObjectStorage.Endpoint, objectKey, bucketName, taskID, err.Error())
				urfm.deleteCacheTaskInMap(taskID)

				return
			}
			defer reader.Close()

			log.Infof("UrchinTaskManager CreateTask to db dstEndpoint %s object %s to sourceBucket %s taskID:%s", urfm.config.ObjectStorage.Endpoint, objectKey, bucketName, taskID)

			if err = urfm.urchinTaskManager.CreateTask(taskID, sourceEndpoint, urfm.config.ObjectStorage.Endpoint, uint64(meta.ContentLength), bucketName+":"+objectKey); err != nil {
				log.Errorf("UrchinTaskManager CreateTask to db err: %s dstEndpoint %s object %s to sourceBucket %s taskID:%s", urfm.config.ObjectStorage.Endpoint, objectKey, bucketName, taskID, err.Error())
				urfm.deleteCacheTaskInMap(taskID)
				return
			}

			log.Infof("pushToOwnBackend dstEndpoint %s object %s to sourceBucket %s", urfm.config.ObjectStorage.Endpoint, objectKey, bucketName)

			if err = objectstorage.PushToOwnBackend(context.Background(), urfm.config.ObjectStorage.Name, urfm.config.ObjectStorage.CacheBucket, objectKey, meta, reader, dstClient); err != nil {
				log.Errorf("pushToOwnBackend dstEndpoint %s object %s to sourceBucket %s taskID:%s failed: %s and delete caching task", urfm.config.ObjectStorage.Endpoint, objectKey, bucketName, taskID, err.Error())
				urfm.deleteCacheTaskInMap(taskID)

				return
			}

			log.Infof("pushToOwnBackend dstEndpoint %s object %s to sourceBucket %s taskID:%s finish and delete caching task", urfm.config.ObjectStorage.Endpoint, objectKey, bucketName, taskID)
			urfm.deleteCacheTaskInMap(taskID)

		}()
	}

	urfm.cachingTaskLock.Unlock()

	log.Infof("cacheObject object content %s and length is %d and content type is %s and digest is %s", fmt.Sprint(meta.ContentLength), meta.ContentType, urlMeta.Digest)

	if urfm.config.ObjectStorage.Name == "sugon" || urfm.config.ObjectStorage.Name == "starlight" {
		dataRootName = filepath.Join("/", strings.ReplaceAll(dataRootName, "-", "/"))
	}
	ctx.JSON(http.StatusOK, gin.H{
		headers.ContentType:                 meta.ContentType,
		headers.ContentLength:               fmt.Sprint(meta.ContentLength),
		config.HeaderUrchinObjectMetaDigest: urlMeta.Digest,
		"StatusCode":                        1,
		"StatusMsg":                         "Caching",
		"TaskID":                            taskID,
		"SignedUrl":                         "",
		"DataRoot":                          dataRootName,
		"DataPath":                          objectKey,
		"DataEndpoint":                      urfm.config.ObjectStorage.Endpoint,
	})
}

// cacheObject uses to download object data in current peer os dir & object cacheBucket & return object url
func (urfm *UrchinFileManager) deleteCacheTaskInMap(taskID string) {
	urfm.cachingTasks.Delete(taskID)

	logger.Info("delete caching task")
	//confirm delete
	for i := 0; i < 3; i++ {
		_, cachingOk := urfm.cachingTasks.Load(taskID)
		if cachingOk {
			logger.Errorf("delete caching task not ok, delete again")
			urfm.cachingTasks.Delete(taskID)
		} else {
			break
		}
	}
}

// CheckObject check the object taskID status
func (urfm *UrchinFileManager) CheckObject(ctx *gin.Context) {
	var params objectstorage.ObjectParams
	if err := ctx.ShouldBindUri(&params); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var query objectstorage.GetObjectQuery
	if err := ctx.ShouldBindQuery(&query); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	bucketEndpoint := strings.SplitN(params.ID, ".", 2)

	if len(bucketEndpoint) < 2 {
		logger.Errorf("checkObject sourceBucket %s is invalid", bucketEndpoint)
		ctx.JSON(http.StatusNotFound, gin.H{"errors": "checkObject datasource" + params.ID + " is invalid"})
		return
	}

	var (
		sourceEndpoint = bucketEndpoint[1]
		bucketName     = bucketEndpoint[0]
		objectKey      = strings.TrimPrefix(params.ObjectKey, string(os.PathSeparator))
		filter         = query.Filter
		err            error
	)

	//1. Check Endpoint
	isInControl := objectstorage.ConfirmDataSourceInBackendPool(urfm.config, sourceEndpoint)
	if !isInControl {
		ctx.JSON(http.StatusForbidden, gin.H{"errors": "DataSource Not In BackendPool:" + http.StatusText(http.StatusForbidden)})
		return
	}
	//2. Check Target Bucket
	if !objectstorage.CheckTargetBucketIsInControl(urfm.config, sourceEndpoint, bucketName) {
		ctx.JSON(http.StatusForbidden, gin.H{"errors": "checkObject,please check datasource bucket & cache bucket is in control & exist in config"})
		return
	}

	// Initialize filter field.
	urlMeta := &commonv1.UrlMeta{Filter: urfm.config.ObjectStorage.Filter}
	if filter != "" {
		urlMeta.Filter = filter
	}

	//3. Check Current Peer CacheBucket is exist datasource Object
	dstClient, err := objectstorage.Client(urfm.config.ObjectStorage.Name,
		urfm.config.ObjectStorage.Region,
		urfm.config.ObjectStorage.Endpoint,
		urfm.config.ObjectStorage.AccessKey,
		urfm.config.ObjectStorage.SecretKey)

	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	anyRes, _, err := retry.Run(ctx, 0.05, 0.2, 3, func() (any, bool, error) {
		ctxSub, cancel := context.WithTimeout(ctx, time.Duration(urfm.config.ObjectStorage.RetryTimeOutSec)*time.Second)
		defer cancel()

		meta, isExist, err := dstClient.GetObjectMetadata(ctxSub, urfm.config.ObjectStorage.CacheBucket, objectKey)
		if isExist && err == nil {
			//exist, skip retry
			return objectstorage.RetryRes{Res: meta, IsExist: isExist}, true, nil
		} else if err != nil && objectstorage.NeedRetry(err) {
			//retry this request, do not cancel this request
			return objectstorage.RetryRes{Res: nil, IsExist: false}, false, err
		} else {
			//retry this request, do not cancel this request
			return objectstorage.RetryRes{Res: nil, IsExist: false}, true, err
		}
	})

	if err != nil {
		logger.Errorf("checkObject, Endpoint %s CacheBucket %s get meta error:%s", urfm.config.ObjectStorage.Endpoint, urfm.config.ObjectStorage.CacheBucket, err.Error())
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	dataRootName := urfm.config.ObjectStorage.CacheBucket
	if urfm.config.ObjectStorage.Name == "sugon" || urfm.config.ObjectStorage.Name == "starlight" {
		dataRootName = filepath.Join("/", strings.ReplaceAll(dataRootName, "-", "/"))
	}

	res := anyRes.(objectstorage.RetryRes)
	if res.IsExist {
		dstMeta := res.Res

		ctx.JSON(http.StatusOK, gin.H{
			headers.ContentType:                 dstMeta.ContentType,
			headers.ContentLength:               fmt.Sprint(dstMeta.ContentLength),
			config.HeaderUrchinObjectMetaDigest: dstMeta.Digest,
			"StatusCode":                        0,
			"StatusMsg":                         "Succeed",
			"TaskID":                            "",
			"SignedUrl":                         "",
			"DataRoot":                          dataRootName,
			"DataPath":                          objectKey,
			"DataEndpoint":                      urfm.config.ObjectStorage.Endpoint,
		})

		return
	} else {
		//4.Check Exist in DstDataCenter's Control Buckets
		for _, bucket := range urfm.config.ObjectStorage.Buckets {
			dataRootName := bucket.Name
			if bucket.Name != urfm.config.ObjectStorage.CacheBucket && bucket.Enable {
				if urfm.config.ObjectStorage.Name == "sugon" || urfm.config.ObjectStorage.Name == "starlight" {
					dataRootName = filepath.Join("/", strings.ReplaceAll(bucket.Name, "-", "/"))
				}
				meta, isExist, err := dstClient.GetObjectMetadata(ctx, bucket.Name, objectKey)

				if isExist {

					ctx.JSON(http.StatusOK, gin.H{
						headers.ContentType:                 meta.ContentType,
						headers.ContentLength:               fmt.Sprint(meta.ContentLength),
						config.HeaderUrchinObjectMetaDigest: meta.Digest,
						"StatusCode":                        0,
						"StatusMsg":                         "Succeed",
						"TaskID":                            "",
						"SignedUrl":                         "",
						"DataRoot":                          dataRootName,
						"DataPath":                          objectKey,
						"DataEndpoint":                      urfm.config.ObjectStorage.Endpoint,
					})

					return
				}

				if err != nil {
					logger.Errorf("Endpoint %s Control Bucket %s get meta error:%s", urfm.config.ObjectStorage.Endpoint, bucket.Name, err.Error())
					ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
					return
				}
			}
		}

		logger.Infof("DstData Endpoint %s Control Buckets not exist object:%s", urfm.config.ObjectStorage.Endpoint, objectKey)
	}

	//DstDataSet Not Exist In DstDataCenter, Move it from DataSource to Dst DataCenter
	//5. Check dataSource is existed this Object
	sourceClient, err := objectstorage.Client(urfm.config.SourceObs.Name,
		urfm.config.SourceObs.Region,
		urfm.config.SourceObs.Endpoint,
		urfm.config.SourceObs.AccessKey,
		urfm.config.SourceObs.SecretKey)

	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	anyRes, _, err = retry.Run(ctx, 0.05, 0.2, 3, func() (any, bool, error) {
		//ctxSub, cancel := context.WithTimeout(ctx, 3*time.Second)
		//defer cancel()

		meta, isExist, err := sourceClient.GetObjectMetadata(ctx, bucketName, objectKey)
		if isExist && err == nil {
			//exist, skip retry
			return objectstorage.RetryRes{Res: meta, IsExist: isExist}, true, nil
		} else if err != nil && objectstorage.NeedRetry(err) {
			//retry this request, do not cancel this request
			return objectstorage.RetryRes{Res: nil, IsExist: false}, false, err
		} else {
			return objectstorage.RetryRes{Res: nil, IsExist: false}, true, err
		}
	})

	if err != nil {
		logger.Errorf("dataSource %s bucket %s object %s err %v ", urfm.config.SourceObs.Endpoint, bucketName, objectKey, err)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}
	res = anyRes.(objectstorage.RetryRes)
	if !res.IsExist {
		logger.Errorf("dataSource %s bucket %s object %s not found", urfm.config.SourceObs.Endpoint, bucketName, objectKey)
		ctx.JSON(http.StatusNotFound, gin.H{"errors": "dataSource:" + urfm.config.SourceObs.Endpoint + "." + bucketName + "/" + objectKey + " not found"})
		return
	}

	sourceMeta := res.Res

	urlMeta.Digest = sourceMeta.Digest

	signURL, err := sourceClient.GetSignURL(ctx, bucketName, objectKey, objectstorage.MethodGet, defaultSignExpireTime)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	signURL, urlMeta = objectstorage.ConvertSignURL(ctx, signURL, urlMeta)

	taskID := idgen.TaskIDV1(signURL, urlMeta)
	log := logger.WithTaskID(taskID)

	_, ok := urfm.cachingTasks.Load(taskID)

	log.Infof("checkObject object %s taskID %s isCaching:%v meta: %s %#v", objectKey, taskID, ok, signURL, urlMeta)

	if ok {
		ctx.JSON(http.StatusOK, gin.H{
			headers.ContentType:                 sourceMeta.ContentType,
			headers.ContentLength:               fmt.Sprint(sourceMeta.ContentLength),
			config.HeaderUrchinObjectMetaDigest: sourceMeta.Digest,
			"StatusCode":                        1,
			"StatusMsg":                         "Caching",
			"TaskID":                            taskID,
			"SignedUrl":                         "",
			"DataRoot":                          dataRootName,
			"DataPath":                          objectKey,
			"DataEndpoint":                      urfm.config.ObjectStorage.Endpoint,
		})
	} else {
		dstMeta, isExist, err := dstClient.GetObjectMetadata(ctx, urfm.config.ObjectStorage.CacheBucket, objectKey)

		if err != nil && !strings.Contains(err.Error(), "404") {
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		if isExist {
			ctx.JSON(http.StatusOK, gin.H{
				headers.ContentType:                 dstMeta.ContentType,
				headers.ContentLength:               fmt.Sprint(dstMeta.ContentLength),
				config.HeaderUrchinObjectMetaDigest: dstMeta.Digest,
				"StatusCode":                        0,
				"StatusMsg":                         "Succeed",
				"TaskID":                            "",
				"SignedUrl":                         "",
				"DataRoot":                          dataRootName,
				"DataPath":                          objectKey,
				"DataEndpoint":                      urfm.config.ObjectStorage.Endpoint,
			})
		} else {
			ctx.JSON(http.StatusOK, gin.H{
				headers.ContentType:                 sourceMeta.ContentType,
				headers.ContentLength:               fmt.Sprint(sourceMeta.ContentLength),
				config.HeaderUrchinObjectMetaDigest: sourceMeta.Digest,
				"StatusCode":                        2,
				"StatusMsg":                         "NotFound",
				"TaskID":                            "",
				"SignedUrl":                         "",
				"DataRoot":                          dataRootName,
				"DataPath":                          objectKey,
				"DataEndpoint":                      urfm.config.ObjectStorage.Endpoint,
			})
		}
	}

	return
}
