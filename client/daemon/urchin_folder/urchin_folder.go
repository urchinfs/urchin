package urchin_folder

import (
	"context"
	commonv1 "d7y.io/api/pkg/apis/common/v1"
	"d7y.io/dragonfly/v2/client/config"
	"d7y.io/dragonfly/v2/client/daemon/peer"
	urchintask "d7y.io/dragonfly/v2/client/daemon/urchin_task"
	logger "d7y.io/dragonfly/v2/internal/dflog"
	"d7y.io/dragonfly/v2/pkg/idgen"
	nethttp "d7y.io/dragonfly/v2/pkg/net/http"
	"d7y.io/dragonfly/v2/pkg/objectstorage"
	"d7y.io/dragonfly/v2/pkg/retry"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/go-http-utils/headers"
	"github.com/pkg/errors"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	RouterGroupBuckets  = "/buckets"
	CoroutineLimiterNum = 8
)

const (
	// defaultSignExpireTime is default expire of sign url.
	//- v2.0.7: fix signed url expired.
	defaultSignExpireTime = 3 * 60 * time.Minute

	GB_1   = 1024 * 1024 * 1024
	MB_500 = 1024 * 1024 * 500
	MB_100 = 1024 * 1024 * 100
	MB_10  = 1024 * 1024 * 10
	MB_1   = 1024 * 1024 * 1
)

type CachingFolderInfo struct {
	isCaching  bool
	objectNums int
}

type UrchinFolderManager struct {
	config            *config.DaemonOption
	peerTaskManager   peer.TaskManager
	peerIDGenerator   peer.IDGenerator
	urchinTaskManager *urchintask.UrchinTaskManager
	cachingTasks      sync.Map
	cachingTaskLock   sync.Locker
	cachingFolders    sync.Map
	cachingFolderLock sync.Locker
}

func NewUrchinFolderManager(cfg *config.DaemonOption, peerTaskManager peer.TaskManager, peerIDGenerator peer.IDGenerator, urchinTaskManager *urchintask.UrchinTaskManager) (*UrchinFolderManager, error) {
	logger.Infof("UrchinFolderManager new and init")

	isControl := objectstorage.CheckAllCacheBucketIsInControl(cfg)
	if !isControl {
		return nil, errors.New("not all cache bucket is in control,please check config")
	}

	return &UrchinFolderManager{
		config:            cfg,
		peerTaskManager:   peerTaskManager,
		peerIDGenerator:   peerIDGenerator,
		urchinTaskManager: urchinTaskManager,
		cachingTasks:      sync.Map{},
		cachingTaskLock:   &sync.Mutex{},
		cachingFolders:    sync.Map{},
		cachingFolderLock: &sync.Mutex{},
	}, nil
}

func (urf *UrchinFolderManager) CacheFolder(ctx *gin.Context) {
	logger.Infof("Cache Urchin Folder entered...")

	var params FolderParams
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
		folderKey      = strings.TrimPrefix(params.FolderKey, string(os.PathSeparator))
		filter         = query.Filter
		rg             *nethttp.Range
		err            error
	)

	if !strings.HasSuffix(folderKey, "/") {
		folderKey += "/"
	}

	// Check Endpoint
	isInControl := objectstorage.ConfirmDataSourceInBackendPool(urf.config, sourceEndpoint)
	if !isInControl {
		ctx.JSON(http.StatusForbidden, gin.H{"errors": "DataSource Not In BackendPool:" + http.StatusText(http.StatusForbidden)})
		return
	}
	// Check Target Bucket
	if !objectstorage.CheckTargetBucketIsInControl(urf.config, sourceEndpoint, bucketName) {
		ctx.JSON(http.StatusForbidden, gin.H{"errors": "please check datasource bucket & cache bucket is in control & exist in config"})
		return
	}

	// Initialize filter field.
	urlMeta := &commonv1.UrlMeta{Filter: urf.config.ObjectStorage.Filter}
	if filter != "" {
		urlMeta.Filter = filter
	}

	sourceClient, err := objectstorage.Client(urf.config.SourceObs.Name,
		urf.config.SourceObs.Region,
		urf.config.SourceObs.Endpoint,
		urf.config.SourceObs.AccessKey,
		urf.config.SourceObs.SecretKey)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	anyRes, _, err := retry.Run(ctx, 0.05, 0.2, 3, func() (any, bool, error) {
		//ctxSub, cancel := context.WithTimeout(ctx, 15*time.Second)
		//defer cancel()

		meta, isExist, err := sourceClient.GetFolderMetadata(ctx, bucketName, folderKey)
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
		logger.Errorf("dataSource %s bucket %s folder %s err %v ", urf.config.SourceObs.Endpoint, bucketName, folderKey, err)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	res := anyRes.(objectstorage.RetryRes)
	if !res.IsExist {
		logger.Errorf("dataSource %s bucket %s folder %s not found", urf.config.SourceObs.Endpoint, bucketName, folderKey)
		ctx.JSON(http.StatusNotFound, gin.H{"errors": "dataSource:" + urf.config.SourceObs.Endpoint + "." + bucketName + "/" + folderKey + " not found"})
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

	signURL, err := sourceClient.GetSignURL(ctx, bucketName, folderKey, objectstorage.MethodGet, defaultSignExpireTime)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	signURL, urlMeta = objectstorage.ConvertSignURL(ctx, signURL, urlMeta)

	taskID := idgen.TaskIDV1(signURL, urlMeta)

	// Check Current Peer CacheBucket is existed datasource Object
	dstClient, err := objectstorage.Client(urf.config.ObjectStorage.Name,
		urf.config.ObjectStorage.Region,
		urf.config.ObjectStorage.Endpoint,
		urf.config.ObjectStorage.AccessKey,
		urf.config.ObjectStorage.SecretKey)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	anyRes, _, err = retry.Run(ctx, 0.05, 0.2, 3, func() (any, bool, error) {
		ctxSub, cancel := context.WithTimeout(ctx, time.Duration(urf.config.ObjectStorage.RetryTimeOutSec)*time.Second)
		defer cancel()

		meta, isExist, err := dstClient.GetFolderMetadata(ctxSub, urf.config.ObjectStorage.CacheBucket, folderKey)
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
		logger.Errorf("Endpoint %s CacheFolder %s get meta error:%s", urf.config.ObjectStorage.Endpoint, urf.config.ObjectStorage.CacheBucket, err.Error())
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	res = anyRes.(objectstorage.RetryRes)
	dataRootName := urf.config.ObjectStorage.CacheBucket
	if urf.config.ObjectStorage.Name == "sugon" || urf.config.ObjectStorage.Name == "starlight" {
		dataRootName = filepath.Join("/", strings.ReplaceAll(dataRootName, "-", "/"))
	}
	if res.IsExist {
		urf.cachingFolderLock.Lock()
		_, ok := urf.cachingFolders.Load(taskID)
		if !ok {
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
				"DataPath":                          folderKey,
				"DataEndpoint":                      urf.config.ObjectStorage.Endpoint,
			})

			urf.cachingFolderLock.Unlock()
			return
		}
		urf.cachingFolderLock.Unlock()

	} else {
		//4.Check Exist in DstDataCenter's Control Buckets
		for _, bucket := range urf.config.ObjectStorage.Buckets {
			if bucket.Name != urf.config.ObjectStorage.CacheBucket && bucket.Enable {
				meta, isExist, err := dstClient.GetFolderMetadata(ctx, bucket.Name, folderKey)
				if isExist {
					dataRootName := bucket.Name
					if urf.config.ObjectStorage.Name == "sugon" || urf.config.ObjectStorage.Name == "starlight" {
						dataRootName = filepath.Join("/", strings.ReplaceAll(dataRootName, "-", "/"))
					}
					ctx.JSON(http.StatusOK, gin.H{
						headers.ContentType:                 meta.ContentType,
						headers.ContentLength:               fmt.Sprint(meta.ContentLength),
						config.HeaderUrchinObjectMetaDigest: meta.Digest,
						"StatusCode":                        0,
						"StatusMsg":                         "Succeed",
						"TaskID":                            "",
						"SignedUrl":                         "",
						"DataRoot":                          dataRootName,
						"DataPath":                          folderKey,
						"DataEndpoint":                      urf.config.ObjectStorage.Endpoint,
					})

					return
				}

				if err != nil {
					logger.Errorf("Endpoint %s Control Folder %s get meta error:%s", urf.config.ObjectStorage.Endpoint, bucket.Name, err.Error())
					ctx.JSON(http.StatusInternalServerError, gin.H{"errors": "Endpoint " + urf.config.ObjectStorage.Endpoint + " Control Folder " + bucket.Name + " get meta error:" + err.Error()})
					return
				}
			}
		}

		logger.Infof("DstData Endpoint %s Control Buckets not exist Folder:%s", urf.config.ObjectStorage.Endpoint, folderKey)
	}

	log := logger.WithTaskID(taskID)

	urf.cachingFolderLock.Lock()
	defer func() {
		urf.cachingFolderLock.Unlock()
	}()
	folderInfo, ok := urf.cachingFolders.Load(taskID)

	log.Infof("cacheFolder folder %s taskID %s isCaching:%v meta: %s %#v", folderKey, taskID, ok, signURL, urlMeta)

	//- value 0 may not be an initial value and conflicts with empty files
	objectNums := -1
	if !ok {
		sourceObjs, err := sourceClient.ListFolderObjects(ctx, bucketName, folderKey)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		objectNums = len(sourceObjs)
		for _, obj := range sourceObjs {
			if strings.HasSuffix(obj.Key, "/") {
				objectNums--
			}
		}

		urf.cachingFolders.Store(taskID, CachingFolderInfo{
			isCaching:  true,
			objectNums: objectNums,
		})
		log.Infof("cacheFolder folder %s, object num %d", folderKey, objectNums)

		go func() {
			objectWg := &sync.WaitGroup{}
			coroutineLimiter := make(chan bool, CoroutineLimiterNum)
			moveObjResultChan := make(chan bool, len(sourceObjs))

			defer func() {
				defer close(coroutineLimiter)
				defer close(moveObjResultChan)

				urf.cachingFolderLock.Lock()
				urf.cachingFolders.Delete(taskID)
				const maxRetryCnt = 3
				for i := 0; i < maxRetryCnt; i++ {
					_, cachingOk := urf.cachingFolders.Load(taskID)
					if cachingOk {
						log.Errorf("delete caching folder not ok, delete again")
						urf.cachingFolders.Delete(taskID)
					} else {
						break
					}

					time.Sleep(time.Millisecond * 100)
				}

				urf.cachingFolderLock.Unlock()
			}()

			processStartTime := time.Now()
			folderMoveResult := true
			if len(sourceObjs) > 0 {
				var traverseMap sync.Map
				var traverseMapLock sync.Mutex
				AsyncCreateFolder := func(ctx context.Context, bucketName, folderName string, isEmptyFolder bool,
					resultOption func(err error)) {
					defer func() {
						objectWg.Done()
						<-coroutineLimiter
					}()

					err = dstClient.CreateFolder(ctx, bucketName, folderName, isEmptyFolder)
					if err != nil {
						log.Infof("cacheFolder folder %s taskID %s create folder err:%v", folderName, taskID, err)
						resultOption(err)
						return
					}

					resultOption(err)
				}

				for _, obj := range sourceObjs {
					objInner := obj

					if strings.HasSuffix(objInner.Key, "/") {
						traverseMapLock.Lock()
						if _, done := traverseMap.Load(objInner.Key); done {
							traverseMapLock.Unlock()
							moveObjResultChan <- true
							continue
						}
						traverseMap.Store(objInner.Key, true)
						traverseMapLock.Unlock()

						emptyDir := true
						for _, objTmp := range sourceObjs {
							if objTmp.Key != objInner.Key && strings.HasPrefix(objTmp.Key, objInner.Key) {
								emptyDir = false
								break
							}
						}

						objectWg.Add(1)
						coroutineLimiter <- true
						go AsyncCreateFolder(ctx, urf.config.ObjectStorage.CacheBucket, objInner.Key, emptyDir, func(err error) {
							if err != nil {
								moveObjResultChan <- false
								log.Infof("cacheFolder bucket %s folder %s, set result chan false", urf.config.ObjectStorage.CacheBucket, objInner.Key)
							} else {
								moveObjResultChan <- true
							}
						})

						continue
					}

					{
						paths := strings.Split(objInner.Key, "/")
						paths = paths[:len(paths)-1]
						for i := 0; i < len(paths); i++ {
							TmpVec := paths[:i+1]
							path := ""
							for _, subPath := range TmpVec {
								path = path + subPath + "/"
							}

							traverseMapLock.Lock()
							if _, done := traverseMap.Load(path); done {
								traverseMapLock.Unlock()
								continue
							}
							traverseMap.Store(path, true)
							traverseMapLock.Unlock()

							objectWg.Add(1)
							coroutineLimiter <- true
							go AsyncCreateFolder(ctx, urf.config.ObjectStorage.CacheBucket, path, false, func(err error) {
								if err != nil {
									log.Infof("cacheFolder bucket %s folder %s, split sub path :%s create failed", urf.config.ObjectStorage.CacheBucket, folderKey, path)
								}
								return
							})
						}
					}

					objectWg.Add(1)
					coroutineLimiter <- true
					go func() {
						err := urf.moveObjectBetweenPeer(ctx, bucketName, objInner.Key, urlMeta, rg, sourceEndpoint, sourceClient, dstClient, objectWg, coroutineLimiter)
						if err != nil {
							moveObjResultChan <- false
							log.Infof("cacheFolder move object failed, bucket %s folder %s object %s", bucketName, folderKey, objInner.Key)
							return
						} else {
							moveObjResultChan <- true
						}

					}()
				}

				idx := 0
				for idx < len(sourceObjs) {
					objResult := <-moveObjResultChan
					if !objResult {
						folderMoveResult = false
						log.Infof("cacheFolder folder %s taskID %s processed failed", folderKey, taskID)
						break
					}

					idx++
				}

			} else {
				err = dstClient.CreateFolder(ctx, urf.config.ObjectStorage.CacheBucket, folderKey, true)
				if err != nil {
					log.Infof("cacheFolder folder %s taskID %s create folder err:%v", folderKey, taskID, err)
					return
				}

			}

			objectWg.Wait()

			if !folderMoveResult {
				err := urf.deleteFolderAndFiles(ctx, urf.config.ObjectStorage.CacheBucket, folderKey, dstClient)
				if err != nil && !strings.Contains(err.Error(), "404") {
					log.Infof("cacheFolder folder %s taskID %s move failed, delete folder err:%v", folderKey, taskID, err)
					return
				}

				log.Infof("cacheFolder folder %s taskID %s delete folder all object ok, meta: %s %#v, cost %vms", folderKey, taskID, urlMeta)
				return
			}

			var processCost = time.Since(processStartTime).Milliseconds()
			log.Infof("cacheFolder folder %s taskID %s processed ok meta: %s %#v, cost %vms", folderKey, taskID, urlMeta, processCost)
		}()

	}

	if objectNums == -1 && ok {
		objectNums = folderInfo.(CachingFolderInfo).objectNums
	}

	log.Infof("cacheFolder folder content %s and length is %d and content type is %s and digest is %s", fmt.Sprint(meta.ContentLength), meta.ContentType, urlMeta.Digest)
	ctx.JSON(http.StatusOK, gin.H{
		headers.ContentType:                 meta.ContentType,
		headers.ContentLength:               fmt.Sprint(meta.ContentLength),
		config.HeaderUrchinObjectMetaDigest: urlMeta.Digest,
		"StatusCode":                        1,
		"StatusMsg":                         "Caching",
		"TaskID":                            taskID,
		"SignedUrl":                         "",
		"DataRoot":                          dataRootName,
		"DataPath":                          folderKey,
		"DataEndpoint":                      urf.config.ObjectStorage.Endpoint,
		"ObjectNums":                        objectNums,
	})

	logger.Infof("Cache Urchin Folder exited...")
	return
}

func (urf *UrchinFolderManager) CheckFolder(ctx *gin.Context) {
	logger.Infof("Check Urchin Folder entered.")

	var params FolderParams
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
		logger.Errorf("checkFolder sourceBucket %s is invalid", bucketEndpoint)
		ctx.JSON(http.StatusNotFound, gin.H{"errors": "checkFolder datasource" + params.ID + " is invalid"})
		return
	}

	var (
		sourceEndpoint = bucketEndpoint[1]
		bucketName     = bucketEndpoint[0]
		folderKey      = strings.TrimPrefix(params.FolderKey, string(os.PathSeparator))
		filter         = query.Filter
		err            error
	)

	if !strings.HasSuffix(folderKey, "/") {
		folderKey += "/"
	}

	//1. Check Endpoint
	isInControl := objectstorage.ConfirmDataSourceInBackendPool(urf.config, sourceEndpoint)
	if !isInControl {
		ctx.JSON(http.StatusForbidden, gin.H{"errors": "DataSource Not In BackendPool:" + http.StatusText(http.StatusForbidden)})
		return
	}
	//2. Check Target Bucket
	if !objectstorage.CheckTargetBucketIsInControl(urf.config, sourceEndpoint, bucketName) {
		ctx.JSON(http.StatusForbidden, gin.H{"errors": "checkObject,please check datasource bucket & cache bucket is in control & exist in config"})
		return
	}

	// Initialize filter field.
	urlMeta := &commonv1.UrlMeta{Filter: urf.config.ObjectStorage.Filter}
	if filter != "" {
		urlMeta.Filter = filter
	}

	//3. Check Current Peer CacheBucket is existed datasource Object
	dstClient, err := objectstorage.Client(urf.config.ObjectStorage.Name,
		urf.config.ObjectStorage.Region,
		urf.config.ObjectStorage.Endpoint,
		urf.config.ObjectStorage.AccessKey,
		urf.config.ObjectStorage.SecretKey)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	{
		//4.Check Exist in DstDataCenter's Control Buckets
		for _, bucket := range urf.config.ObjectStorage.Buckets {
			if bucket.Name != urf.config.ObjectStorage.CacheBucket && bucket.Enable {
				anyRes, _, err := retry.Run(ctx, 0.05, 0.2, 3, func() (any, bool, error) {
					ctxSub, cancel := context.WithTimeout(ctx, time.Duration(urf.config.ObjectStorage.RetryTimeOutSec)*time.Second)
					defer cancel()

					meta, isExist, err := dstClient.GetFolderMetadata(ctxSub, bucket.Name, folderKey)
					if isExist && err == nil {
						//exist, skip retry
						return objectstorage.RetryRes{Res: meta, IsExist: isExist}, true, nil
					} else if err != nil && objectstorage.NeedRetry(err) {
						//retry this request, do not cancel this request
						return objectstorage.RetryRes{Res: nil, IsExist: false}, false, err
					} else {
						//do not retry this request
						return objectstorage.RetryRes{Res: nil, IsExist: false}, true, err
					}
				})

				if err != nil {
					logger.Errorf("Endpoint %s Control Bucket %s get meta error:%s", urf.config.ObjectStorage.Endpoint, bucket.Name, err.Error())
					ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
					return
				}

				res := anyRes.(objectstorage.RetryRes)
				if res.IsExist {
					meta := res.Res
					//meta := anyMeta.(*objectstorage.ObjectMetadata)
					dataRootName := bucket.Name
					if urf.config.ObjectStorage.Name == "sugon" || urf.config.ObjectStorage.Name == "starlight" {
						dataRootName = filepath.Join("/", strings.ReplaceAll(dataRootName, "-", "/"))
					}
					ctx.JSON(http.StatusOK, gin.H{
						headers.ContentType:                 meta.ContentType,
						headers.ContentLength:               fmt.Sprint(meta.ContentLength),
						config.HeaderUrchinObjectMetaDigest: meta.Digest,
						"StatusCode":                        0,
						"StatusMsg":                         "Succeed",
						"TaskID":                            "",
						"SignedUrl":                         "",
						"DataRoot":                          dataRootName,
						"DataPath":                          folderKey,
						"DataEndpoint":                      urf.config.ObjectStorage.Endpoint,
					})

					return
				}

			}
		}

		logger.Infof("DstData Endpoint %s Control Buckets not exist folder:%s", urf.config.ObjectStorage.Endpoint, folderKey)
	}

	//DstDataSet Not Exist In DstDataCenter, Move it from DataSource to Dst DataCenter
	//5. Check dataSource is existed this Object
	sourceClient, err := objectstorage.Client(urf.config.SourceObs.Name,
		urf.config.SourceObs.Region,
		urf.config.SourceObs.Endpoint,
		urf.config.SourceObs.AccessKey,
		urf.config.SourceObs.SecretKey)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	anyRes, _, err := retry.Run(ctx, 0.05, 0.2, 3, func() (any, bool, error) {
		//ctxSub, cancel := context.WithTimeout(ctx, 15*time.Second)
		//defer cancel()

		meta, isExist, err := sourceClient.GetFolderMetadata(ctx, bucketName, folderKey)
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
		logger.Errorf("dataSource %s bucket %s folder %s err %v ", urf.config.SourceObs.Endpoint, bucketName, folderKey, err)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	res := anyRes.(objectstorage.RetryRes)
	if !res.IsExist {
		logger.Errorf("dataSource %s bucket %s folder %s not found", urf.config.SourceObs.Endpoint, bucketName, folderKey)
		ctx.JSON(http.StatusNotFound, gin.H{"errors": "dataSource:" + urf.config.SourceObs.Endpoint + "." + bucketName + "/" + folderKey + " not found"})
		return
	}

	sourceMeta := res.Res

	//sourceMeta, isExist, err := sourceClient.GetFolderMetadata(ctx, bucketName, folderKey)
	//if !isExist {
	//	ctx.JSON(http.StatusNotFound, gin.H{"errors": "dataSource:" + urf.config.SourceObs.Endpoint + "." + bucketName + "/" + folderKey + " not found"})
	//	return
	//}
	//
	//if err != nil {
	//	ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
	//	return
	//}

	urlMeta.Digest = sourceMeta.Digest
	signURL, err := sourceClient.GetSignURL(ctx, bucketName, folderKey, objectstorage.MethodGet, defaultSignExpireTime)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	signURL, urlMeta = objectstorage.ConvertSignURL(ctx, signURL, urlMeta)

	taskID := idgen.TaskIDV1(signURL, urlMeta)
	log := logger.WithTaskID(taskID)

	urf.cachingFolderLock.Lock()
	defer func() {
		urf.cachingFolderLock.Unlock()
	}()

	folderInfo, ok := urf.cachingFolders.Load(taskID)
	log.Infof("checkFolder folder %s taskID %s isCaching:%v meta: %s %#v", folderKey, taskID, ok, signURL, urlMeta)
	dataRootName := urf.config.ObjectStorage.CacheBucket
	if urf.config.ObjectStorage.Name == "sugon" || urf.config.ObjectStorage.Name == "starlight" {
		dataRootName = filepath.Join("/", strings.ReplaceAll(dataRootName, "-", "/"))
	}
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
			"DataPath":                          folderKey,
			"DataEndpoint":                      urf.config.ObjectStorage.Endpoint,
			"ObjectNums":                        folderInfo.(CachingFolderInfo).objectNums,
		})
	} else {
		dstMeta, isExist, err := dstClient.GetFolderMetadata(ctx, urf.config.ObjectStorage.CacheBucket, folderKey)
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
				"DataPath":                          folderKey,
				"DataEndpoint":                      urf.config.ObjectStorage.Endpoint,
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
				"DataPath":                          folderKey,
				"DataEndpoint":                      urf.config.ObjectStorage.Endpoint,
			})
		}
	}

	logger.Infof("Check Urchin Folder exited.")
	return
}

func (urf *UrchinFolderManager) moveObjectBetweenPeer(ctx *gin.Context,
	bucketName, objectKey string,
	urlMeta *commonv1.UrlMeta,
	rg *nethttp.Range,
	sourceEndpoint string,
	srcClient objectstorage.ObjectStorage,
	dstClient objectstorage.ObjectStorage,
	objectWg *sync.WaitGroup,
	coroutineLimiter chan bool) error {
	defer func() {
		if objectWg != nil {
			objectWg.Done()
		}

		if coroutineLimiter != nil {
			<-coroutineLimiter
		}
	}()

	signURL, err := srcClient.GetSignURL(ctx, bucketName, objectKey, objectstorage.MethodGet, defaultSignExpireTime)
	if err != nil {
		return err
	}

	signURL, urlMeta = objectstorage.ConvertSignURL(ctx, signURL, urlMeta)

	taskID := idgen.TaskIDV1(signURL, urlMeta)
	log := logger.WithTaskID(taskID)
	println("signUrl:", signURL)
	var tickTimer *time.Ticker
	rand.Seed(time.Now().UnixNano())
	meta, isExist, metaErr := srcClient.GetObjectMetadata(ctx, bucketName, objectKey)
	if metaErr == nil && isExist {
		if meta.ContentLength > GB_1 {
			tickTimer = time.NewTicker(120 * time.Second)
			time.Sleep(time.Second * (5 + time.Duration(rand.Intn(5))))
		} else if meta.ContentLength > MB_500 {
			tickTimer = time.NewTicker(60 * time.Second)
			time.Sleep(time.Second * (4 + time.Duration(rand.Intn(3))))
		} else if meta.ContentLength > MB_100 {
			tickTimer = time.NewTicker(50 * time.Second)
			time.Sleep(time.Second * (3 + time.Duration(rand.Intn(3))))
		} else if meta.ContentLength > MB_10 {
			tickTimer = time.NewTicker(40 * time.Second)
			time.Sleep(time.Second * (2 + time.Duration(rand.Intn(3))))
		} else {
			tickTimer = time.NewTicker(30 * time.Second)
			time.Sleep(time.Second * (1 + time.Duration(rand.Intn(2))))
		}
	}

	log.Infof("StartStreamTask dstEndpoint %s begin move object %s to sourceBucket %s", urf.config.ObjectStorage.Endpoint, objectKey, bucketName)
	reader, _, subscribeFunc, unSubscribeFunc, err := urf.peerTaskManager.StartStreamTask(ctx, &peer.StreamTaskRequest{
		URL:                         signURL,
		URLMeta:                     urlMeta,
		Range:                       rg,
		PeerID:                      urf.peerIDGenerator.PeerID(),
		NeedPieceDownloadStatusChan: true,
	})
	if err != nil {
		log.Errorf("StartStreamTask dstEndpoint %s object %s to sourceBucket %s taskID:%s err: %s and delete caching task", urf.config.ObjectStorage.Endpoint, objectKey, bucketName, taskID, err.Error())
		return err
	}
	defer reader.Close()

	log.Infof("UrchinTaskManager CreateTask to db dstEndpoint %s object %s to sourceBucket %s taskID:%s", urf.config.ObjectStorage.Endpoint, objectKey, bucketName, taskID)

	if err = urf.urchinTaskManager.CreateTask(taskID, sourceEndpoint, urf.config.ObjectStorage.Endpoint, uint64(meta.ContentLength), bucketName+":"+objectKey); err != nil {
		log.Errorf("UrchinTaskManager CreateTask to db err: %s dstEndpoint %s object %s to sourceBucket %s taskID:%s", urf.config.ObjectStorage.Endpoint, objectKey, bucketName, taskID, err.Error())
		return err
	}

	if metaErr == nil && isExist {
		if subscribeFunc != nil && unSubscribeFunc != nil && meta.ContentLength > 1024*1024*10 {
			pieceChan := subscribeFunc()
		loop:
			for {
				select {
				case piece := <-pieceChan:
					if piece.OrderedNum >= 0 {
						log.Infof("pushToOwnBackend dstEndpoint %s bucket:%s-object:%s, got first piece info: Num:%d,OrderedNum:%d,RangeSize:%d",
							urf.config.ObjectStorage.Endpoint, bucketName, objectKey, piece.Num, piece.OrderedNum, piece.RangeSize)
						break loop
					} else {
						log.Debugf("pushToOwnBackend dstEndpoint %s bucket:%s-object:%s, wait for first piece info: Num:%d,OrderedNum:%d,RangeSize:%d",
							urf.config.ObjectStorage.Endpoint, bucketName, objectKey, piece.Num, piece.OrderedNum, piece.RangeSize)
						continue
					}
				case <-tickTimer.C:
					log.Infof("pushToOwnBackend dstEndpoint %s got first piece timeout...", urf.config.ObjectStorage.Endpoint, bucketName, objectKey)
					return errors.New("wait first package timeout")

				}
			}
			unSubscribeFunc(pieceChan)
		} else if subscribeFunc == nil {
			log.Infof("piece chan subscribeFunc is nil, bucket:%s-object:%s", bucketName, objectKey)
		}

	} else {
		log.Infof("GetObjectMetadata bucket:%s-object:%s fail or not exist", bucketName, objectKey)
		time.Sleep(40 * time.Second)
	}

	log.Infof("pushToOwnBackend dstEndpoint %s object %s to sourceBucket %s", urf.config.ObjectStorage.Endpoint, objectKey, bucketName)
	if err = objectstorage.PushToOwnBackend(context.Background(), urf.config.ObjectStorage.Name, urf.config.ObjectStorage.CacheBucket, objectKey, meta, reader, dstClient); err != nil {
		log.Errorf("pushToOwnBackend dstEndpoint %s object %s to sourceBucket %s taskID:%s failed: %s and delete caching task", urf.config.ObjectStorage.Endpoint, objectKey, bucketName, taskID, err.Error())

		return err
	}

	log.Infof("pushToOwnBackend dstEndpoint %s object %s to sourceBucket %s taskID:%s finish and delete caching task", urf.config.ObjectStorage.Endpoint, objectKey, bucketName, taskID)

	return nil
}

func (urf *UrchinFolderManager) deleteFolderAndFiles(ctx *gin.Context, bucketName, folderName string, dstClient objectstorage.ObjectStorage) error {
	sourceObjs, err := dstClient.ListFolderObjects(ctx, bucketName, folderName)
	if err != nil {
		return err
	}

	if len(sourceObjs) <= 0 {
		return nil
	}

	err = dstClient.DeleteObjects(ctx, bucketName, sourceObjs)
	if err != nil {
		return err
	}

	return nil
}

// DestroyFolder uses to delete folder data.
func (urf *UrchinFolderManager) DestroyFolder(ctx *gin.Context) {
	var params FolderParams
	if err := ctx.ShouldBindUri(&params); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		bucketName = params.ID
		folderKey  = strings.TrimPrefix(params.FolderKey, string(os.PathSeparator))
	)

	client, err := objectstorage.Client(urf.config.ObjectStorage.Name,
		urf.config.ObjectStorage.Region,
		urf.config.ObjectStorage.Endpoint,
		urf.config.ObjectStorage.AccessKey,
		urf.config.ObjectStorage.SecretKey)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	logger.Infof("destroy folder %s in bucket %s", folderKey, bucketName)
	_, _, err = retry.Run(ctx, 0.05, 0.2, 3, func() (any, bool, error) {
		_, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		if err := urf.deleteFolderAndFiles(ctx, bucketName, folderKey, client); err != nil {
			if objectstorage.NeedRetry(err) {
				return nil, false, err
			}

			logger.Infof("deleteFolderAndFiles failed, folderKey %s in bucket %s", folderKey, bucketName)
			return nil, true, err
		}
		return nil, false, nil
	})
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	ctx.Status(http.StatusOK)
	return
}
