//go:generate mockgen -destination mocks/objectstorage_mock.go -source objectstorage.go -package mocks

package objectstorage

import (
	"bytes"
	"context"
	"d7y.io/api/pkg/apis/scheduler/v1"
	urchindataset "d7y.io/dragonfly/v2/client/daemon/urchin_dataset"
	urchindatasetv "d7y.io/dragonfly/v2/client/daemon/urchin_dataset_vesion"
	urchinfile "d7y.io/dragonfly/v2/client/daemon/urchin_file"
	urchinfolder "d7y.io/dragonfly/v2/client/daemon/urchin_folder"
	urchinpeers "d7y.io/dragonfly/v2/client/daemon/urchin_peers"
	urchintask "d7y.io/dragonfly/v2/client/daemon/urchin_task"
	"d7y.io/dragonfly/v2/pkg/retry"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-http-utils/headers"
	ginprometheus "github.com/mcuadros/go-gin-prometheus"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	commonv1 "d7y.io/api/pkg/apis/common/v1"

	"d7y.io/dragonfly/v2/client/config"
	"d7y.io/dragonfly/v2/client/daemon/peer"
	"d7y.io/dragonfly/v2/client/daemon/storage"
	logger "d7y.io/dragonfly/v2/internal/dflog"
	"d7y.io/dragonfly/v2/pkg/digest"
	"d7y.io/dragonfly/v2/pkg/idgen"
	nethttp "d7y.io/dragonfly/v2/pkg/net/http"
	"d7y.io/dragonfly/v2/pkg/objectstorage"
	pkgstrings "d7y.io/dragonfly/v2/pkg/strings"
)

const (
	// AsyncWriteBack writes the object asynchronously to the backend.
	AsyncWriteBack = iota

	// WriteBack writes the object synchronously to the backend.
	WriteBack

	// Ephemeral only writes the object to the dfdaemon.
	// It is only provided for creating temporary objects between peers,
	// and users are not allowed to use this mode.
	Ephemeral
)

const (
	PrometheusSubsystemName = "dragonfly_dfdaemon_object_stroage"
	OtelServiceName         = "dragonfly-dfdaemon-object-storage"
)

const (
	RouterGroupBuckets = "/buckets"
)

var GinLogFileName = "gin-object-stroage.log"

const (
	// defaultSignExpireTime is default expire of sign url.
	defaultSignExpireTime = 5 * time.Minute
)

// ObjectStorage is the interface used for object storage server.
type ObjectStorage interface {
	// Started object storage server.
	Serve(lis net.Listener, port int) error

	// Stop object storage server.
	Stop() error
}

// objectStorage provides object storage function.
type objectStorage struct {
	*http.Server
	config              *config.DaemonOption
	dynconfig           config.Dynconfig
	peerTaskManager     peer.TaskManager
	storageManager      storage.Manager
	peerIDGenerator     peer.IDGenerator
	urchinPeer          *urchinpeers.UrchinPeer
	urchinTaskManager   *urchintask.UrchinTaskManager
	urchinFileManager   *urchinfile.UrchinFileManager
	urchinFolderManager *urchinfolder.UrchinFolderManager
}

// New returns a new ObjectStorage instence.
func New(cfg *config.DaemonOption, dynconfig config.Dynconfig, peerHost *scheduler.PeerHost, peerTaskManager peer.TaskManager, storageManager storage.Manager, logDir string) (ObjectStorage, error) {
	urchinTaskManager, err := urchintask.NewUrchinTaskManager(cfg, peerTaskManager)
	if err != nil {
		logger.Errorf("NewUrchinTaskManager err:%v", err)
		return nil, err
	}

	peerIDGenerator := peer.NewPeerIDGenerator(cfg.Host.AdvertiseIP.String())
	urchinFileManager, err := urchinfile.NewUrchinFileManager(cfg, dynconfig, peerTaskManager, storageManager, peerIDGenerator, urchinTaskManager)
	if err != nil {
		logger.Errorf("NewUrchinFileManager err:%v", err)
		return nil, err
	}

	urchinFolderManager, err := urchinfolder.NewUrchinFolderManager(cfg, peerTaskManager, peerIDGenerator, urchinTaskManager)
	if err != nil {
		logger.Errorf("NewUrchinFolderManager err:%v", err)
		return nil, err
	}

	urchindataset.SetDataSetConfInfo(cfg, dynconfig)

	o := &objectStorage{
		config:              cfg,
		dynconfig:           dynconfig,
		peerTaskManager:     peerTaskManager,
		storageManager:      storageManager,
		peerIDGenerator:     peerIDGenerator,
		urchinPeer:          urchinpeers.NewPeer(peerHost, &cfg.Storage),
		urchinTaskManager:   urchinTaskManager,
		urchinFileManager:   urchinFileManager,
		urchinFolderManager: urchinFolderManager,
	}
	router := o.initRouter(cfg, logDir)
	o.Server = &http.Server{
		Handler: router,
	}

	return o, nil
}

// Serve Started object storage server.
func (o *objectStorage) Serve(lis net.Listener, port int) error {
	o.urchinPeer.PeerPort = port
	return o.Server.Serve(lis)
}

// Stop object storage server.
func (o *objectStorage) Stop() error {
	return o.Server.Shutdown(context.Background())
}

// Initialize router of gin.
func (o *objectStorage) initRouter(cfg *config.DaemonOption, logDir string) *gin.Engine {
	// Set mode
	if !cfg.Verbose {
		gin.SetMode(gin.ReleaseMode)
	}

	// Logging to a file
	if !cfg.Console {
		gin.DisableConsoleColor()
		logDir := filepath.Join(logDir, "daemon")
		f, _ := os.Create(filepath.Join(logDir, GinLogFileName))
		gin.DefaultWriter = io.MultiWriter(f)
	}

	r := gin.New()

	// Middleware
	r.Use(gin.Logger())
	r.Use(gin.Recovery())

	// Prometheus metrics
	p := ginprometheus.NewPrometheus(PrometheusSubsystemName)
	// Prometheus metrics need to reduce label,
	// refer to https://prometheus.io/docs/practices/instrumentation/#do-not-overuse-labels.
	p.ReqCntURLLabelMappingFn = func(c *gin.Context) string {
		if strings.HasPrefix(c.Request.URL.Path, RouterGroupBuckets) {
			return RouterGroupBuckets
		}

		return c.Request.URL.Path
	}
	p.Use(r)

	// Opentelemetry
	if cfg.Options.Telemetry.Jaeger != "" {
		r.Use(otelgin.Middleware(OtelServiceName))
	}

	// Health Check.
	r.GET("/healthy", o.getHealth)

	// Buckets
	b := r.Group(RouterGroupBuckets)
	b.HEAD(":id/objects/*object_key", o.headObject)
	b.POST(":id/cache_object/*object_key", o.urchinFileManager.CacheObject)
	b.GET(":id/check_object/*object_key", o.urchinFileManager.CheckObject)
	b.GET(":id/objects/*object_key", o.getObject)
	b.DELETE(":id/objects/*object_key", o.destroyObject)
	b.PUT(":id/objects/*object_key", o.putObject)

	b.POST(":id/cache_folder/*folder_key", o.urchinFolderManager.CacheFolder)
	b.GET(":id/check_folder/*folder_key", o.urchinFolderManager.CheckFolder)

	api := r.Group("/api/v1")
	api.GET("/peers", urchinpeers.GetUrchinPeers)
	api.GET("/peer/:peer_id", o.urchinPeer.GetPeer)

	api.POST("/dataset", urchindataset.CreateDataSet)
	api.PATCH("/dataset/:dataset_id", urchindataset.UpdateDataSet)
	api.DELETE("/dataset/:dataset_id", urchindataset.DeleteDataSet)
	api.GET("/dataset/:dataset_id", urchindataset.GetDataSet)
	api.GET("/datasets", urchindataset.ListDataSets)

	api.POST("/dataset/:dataset_id/version", urchindatasetv.CreateDataSetVersion)
	api.DELETE("/dataset/:dataset_id/version/:version_id", urchindatasetv.DeleteDataSetVersion)
	api.GET("/dataset/:dataset_id/version/:version_id", urchindatasetv.GetDataSetVersion)
	api.GET("/dataset/:dataset_id/versions", urchindatasetv.ListDataSetVersions)

	api.GET("/task/:task_id", o.urchinTaskManager.GetTask)
	api.GET("/tasks", o.urchinTaskManager.GetTasks)
	api.PUT("/file/upload", o.urchinFileManager.UploadFile)
	api.GET("/file/stat", o.urchinFileManager.StatFile)

	return r
}

// getHealth uses to check server health.
func (o *objectStorage) getHealth(ctx *gin.Context) {
	ctx.JSON(http.StatusOK, http.StatusText(http.StatusOK))
}

// headObject uses to head object.
func (o *objectStorage) headObject(ctx *gin.Context) {
	var params objectstorage.ObjectParams
	if err := ctx.ShouldBindUri(&params); err != nil {
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
	)

	//1. Check Endpoint
	isInControl := objectstorage.ConfirmDataSourceInBackendPool(o.config, sourceEndpoint)
	if !isInControl {
		ctx.JSON(http.StatusForbidden, gin.H{"errors": http.StatusText(http.StatusForbidden)})
		return
	}
	//2. Check Target Bucket
	if !objectstorage.CheckTargetBucketIsInControl(o.config, sourceEndpoint, bucketName) {
		ctx.JSON(http.StatusForbidden, gin.H{"errors": "please check datasource bucket & cache bucket is in control & exist in config"})
		return
	}

	//3. Check Object
	client, err := objectstorage.Client(o.config.SourceObs.Name,
		o.config.SourceObs.Region,
		o.config.SourceObs.Endpoint,
		o.config.SourceObs.AccessKey,
		o.config.SourceObs.SecretKey)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	anyMeta, isExist, err := retry.Run(ctx, 0.05, 0.2, 3, func() (any, bool, error) {
		ctxSub, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		meta, isExist, err := client.GetObjectMetadata(ctxSub, bucketName, objectKey)

		if isExist {
			//exist, skip retry
			return meta, isExist, nil
		} else if err != nil && objectstorage.NeedRetry(err) {
			//retry this request, do not cancel this request
			return nil, false, err
		} else if err != nil && strings.Contains(err.Error(), "404") {
			//not exist, err is Known, set err = nil to skip retry
			return nil, isExist, nil
		} else {
			//retry this request, do not cancel this request
			return nil, false, err
		}
	})

	if err != nil && !strings.Contains(err.Error(), "404") {
		logger.Errorf("Endpoint %s Bucket %s get meta error:%s", o.config.SourceObs.Endpoint, bucketName, err.Error())
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	if !isExist {
		ctx.JSON(http.StatusNotFound, gin.H{"errors": http.StatusText(http.StatusNotFound)})
		return
	}

	meta := anyMeta.(*objectstorage.ObjectMetadata)

	ctx.Header(headers.ContentDisposition, meta.ContentDisposition)
	ctx.Header(headers.ContentEncoding, meta.ContentEncoding)
	ctx.Header(headers.ContentLanguage, meta.ContentLanguage)
	ctx.Header(headers.ContentLength, fmt.Sprint(meta.ContentLength))
	ctx.Header(headers.ContentType, meta.ContentType)
	ctx.Header(headers.ETag, meta.ETag)
	ctx.Header(config.HeaderUrchinObjectMetaDigest, meta.Digest)

	ctx.Status(http.StatusOK)
	return
}

// getObject uses to download object data.
func (o *objectStorage) getObject(ctx *gin.Context) {
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
	isInControl := objectstorage.ConfirmDataSourceInBackendPool(o.config, sourceEndpoint)
	if !isInControl {
		ctx.JSON(http.StatusForbidden, gin.H{"errors": http.StatusText(http.StatusForbidden)})
		return
	}
	//2. Check Target Bucket
	if !objectstorage.CheckTargetBucketIsInControl(o.config, sourceEndpoint, bucketName) {
		ctx.JSON(http.StatusForbidden, gin.H{"errors": http.StatusText(http.StatusForbidden)})
		return
	}

	// Initialize filter field.
	urlMeta := &commonv1.UrlMeta{Filter: o.config.ObjectStorage.Filter}
	if filter != "" {
		urlMeta.Filter = filter
	}

	//3. Check dataSource Object
	sourceClient, err := objectstorage.Client(o.config.SourceObs.Name,
		o.config.SourceObs.Region,
		o.config.SourceObs.Endpoint,
		o.config.SourceObs.AccessKey,
		o.config.SourceObs.SecretKey)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	//3. Check datasource Object
	anyMeta, isExist, err := retry.Run(ctx, 0.05, 0.2, 3, func() (any, bool, error) {
		ctxSub, cancel := context.WithTimeout(ctx, time.Duration(o.config.ObjectStorage.RetryTimeOutSec)*time.Second)
		defer cancel()

		meta, isExist, err := sourceClient.GetObjectMetadata(ctxSub, bucketName, objectKey)

		if isExist {
			//exist, skip retry
			return meta, isExist, nil
		} else if err != nil && objectstorage.NeedRetry(err) {
			//retry this request, do not cancel this request
			return nil, false, err
		} else if err != nil && strings.Contains(err.Error(), "404") {
			//not exist, err is Known, set err = nil to skip retry
			return nil, isExist, nil
		} else {
			//retry this request, do not cancel this request
			return nil, false, err
		}
	})

	if !isExist {
		ctx.JSON(http.StatusNotFound, gin.H{"errors": http.StatusText(http.StatusNotFound)})
		return
	}

	if err != nil && !strings.Contains(err.Error(), "404") {
		logger.Errorf("datasource Endpoint %s sourceBucket %s get meta error:%s", o.config.SourceObs.Endpoint, bucketName, err.Error())
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	meta := anyMeta.(*objectstorage.ObjectMetadata)

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

		// When the request has a range header,
		// there is no need to calculate md5, set this value to empty.
		urlMeta.Digest = ""
	}

	signURL, err := sourceClient.GetSignURL(ctx, bucketName, objectKey, objectstorage.MethodGet, defaultSignExpireTime)

	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	signURL, urlMeta = objectstorage.ConvertSignURL(ctx, signURL, urlMeta)
	taskID := idgen.TaskIDV1(signURL, urlMeta)
	log := logger.WithTaskID(taskID)
	log.Infof("get object %s meta: %s %#v", objectKey, signURL, urlMeta)

	reader, attr, _, _, err := o.peerTaskManager.StartStreamTask(ctx, &peer.StreamTaskRequest{
		URL:     signURL,
		URLMeta: urlMeta,
		Range:   rg,
		PeerID:  o.peerIDGenerator.PeerID(),
	})
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}
	defer reader.Close()

	var contentLength int64 = -1
	if l, ok := attr[headers.ContentLength]; ok {
		if i, err := strconv.ParseInt(l, 10, 64); err == nil {
			contentLength = i
		}
	}

	log.Infof("object content length is %d and content type is %s", contentLength, attr[headers.ContentType])
	ctx.DataFromReader(http.StatusOK, contentLength, attr[headers.ContentType], reader, nil)
}

// destroyObject uses to delete object data.
func (o *objectStorage) destroyObject(ctx *gin.Context) {
	var params objectstorage.ObjectParams
	if err := ctx.ShouldBindUri(&params); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		bucketName = params.ID
		objectKey  = strings.TrimPrefix(params.ObjectKey, string(os.PathSeparator))
	)

	client, err := objectstorage.Client(o.config.ObjectStorage.Name,
		o.config.ObjectStorage.Region,
		o.config.ObjectStorage.Endpoint,
		o.config.ObjectStorage.AccessKey,
		o.config.ObjectStorage.SecretKey)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	logger.Infof("destroy object %s in bucket %s", objectKey, bucketName)
	_, _, err = retry.Run(ctx, 0.05, 0.2, 3, func() (any, bool, error) {
		ctxSub, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		if err := client.DeleteObject(ctxSub, bucketName, objectKey); err != nil {
			if objectstorage.NeedRetry(err) {
				return nil, false, err
			}

			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return nil, true, err
		}
		return nil, false, nil
	})
	if err != nil {
		return
	}

	ctx.Status(http.StatusOK)
	return
}

// putObject uses to upload object data.
func (o *objectStorage) putObject(ctx *gin.Context) {
	var params objectstorage.ObjectParams
	if err := ctx.ShouldBindUri(&params); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var form PutObjectRequset
	if err := ctx.ShouldBind(&form); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		bucketName  = params.ID
		objectKey   = strings.TrimPrefix(params.ObjectKey, string(os.PathSeparator))
		mode        = form.Mode
		filter      = form.Filter
		maxReplicas = form.MaxReplicas
		fileHeader  = form.File
	)

	//1. Check bingding endpoint Bucket is Control
	if !objectstorage.CheckTargetBucketIsInControl(o.config, o.config.ObjectStorage.Endpoint, bucketName) {
		ctx.JSON(http.StatusForbidden, gin.H{"errors": http.StatusText(http.StatusForbidden)})
		return
	}

	dstClient, err := objectstorage.Client(o.config.ObjectStorage.Name,
		o.config.ObjectStorage.Region,
		o.config.ObjectStorage.Endpoint,
		o.config.ObjectStorage.AccessKey,
		o.config.ObjectStorage.SecretKey)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	p, _, err := retry.Run(ctx, 0.05, 0.2, 3, func() (any, bool, error) {
		ctxSub, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		signURL, err := dstClient.GetSignURL(ctxSub, bucketName, objectKey, objectstorage.MethodGet, defaultSignExpireTime)
		if err != nil {
			if objectstorage.NeedRetry(err) {
				return nil, false, err
			}

			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return nil, true, err
		}
		return signURL, false, nil
	})
	if err != nil {
		return
	}
	signURL := p.(string)

	// Initialize url meta.
	urlMeta := &commonv1.UrlMeta{Filter: o.config.ObjectStorage.Filter}
	dgst := o.md5FromFileHeader(fileHeader)
	urlMeta.Digest = dgst.String()
	if filter != "" {
		urlMeta.Filter = filter
	}

	// Initialize max replicas.
	if maxReplicas == 0 {
		maxReplicas = o.config.ObjectStorage.MaxReplicas
	}
	signURL, urlMeta = objectstorage.ConvertSignURL(ctx, signURL, urlMeta)
	// Initialize task id and peer id.
	taskID := idgen.TaskIDV1(signURL, urlMeta)
	peerID := o.peerIDGenerator.PeerID()

	log := logger.WithTaskAndPeerID(taskID, peerID)
	log.Infof("upload object %s meta: %s %#v", objectKey, signURL, urlMeta)

	// Import object to local storage.
	log.Infof("import object %s to local storage", objectKey)
	if err := o.importObjectToLocalStorage(ctx, taskID, peerID, fileHeader); err != nil {
		log.Error(err)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	// Announce peer information to scheduler.
	log.Info("announce peer to scheduler")
	if err := o.peerTaskManager.AnnouncePeerTask(ctx, storage.PeerTaskMetadata{
		TaskID: taskID,
		PeerID: peerID,
	}, signURL, commonv1.TaskType_DfStore, urlMeta); err != nil {
		log.Error(err)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	// Handle task for backend.
	switch mode {
	case Ephemeral:
		ctx.Status(http.StatusOK)
		return
	case WriteBack:
		// Import object to seed peer.
		go func() {
			if err := o.importObjectToSeedPeers(context.Background(), bucketName, objectKey, urlMeta.Filter, Ephemeral, fileHeader, maxReplicas, log); err != nil {
				log.Errorf("import object %s to seed peers failed: %s", objectKey, err)
			}
		}()

		// Import object to object storage.
		log.Infof("import object %s to bucket %s", objectKey, bucketName)
		if err := o.importObjectToBackend(ctx, o.config.ObjectStorage.Name, bucketName, objectKey, dgst, fileHeader, dstClient); err != nil {
			log.Error(err)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}

		ctx.Status(http.StatusOK)
		return
	case AsyncWriteBack:
		// Import object to seed peer.
		go func() {
			if err := o.importObjectToSeedPeers(context.Background(), bucketName, objectKey, urlMeta.Filter, Ephemeral, fileHeader, maxReplicas, log); err != nil {
				log.Errorf("import object %s to seed peers failed: %s", objectKey, err)
			}
		}()

		// Import object to object storage.
		go func() {
			log.Infof("import object %s to bucket %s", objectKey, bucketName)
			if err := o.importObjectToBackend(context.Background(), o.config.ObjectStorage.Name, bucketName, objectKey, dgst, fileHeader, dstClient); err != nil {
				log.Errorf("import object %s to bucket %s failed: %s", objectKey, bucketName, err.Error())
				return
			}
		}()

		ctx.Status(http.StatusOK)
		return
	}

	ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": fmt.Sprintf("unknow mode %d", mode)})
	return
}

// getAvailableSeedPeer uses to calculate md5 with file header.
func (o *objectStorage) md5FromFileHeader(fileHeader *multipart.FileHeader) *digest.Digest {
	f, err := fileHeader.Open()
	if err != nil {
		return nil
	}
	defer f.Close()

	return digest.New(digest.AlgorithmMD5, digest.MD5FromReader(f))
}

// importObjectToBackend uses to import object to backend.
func (o *objectStorage) importObjectToBackend(ctx context.Context, storageName, bucketName, objectKey string, dgst *digest.Digest, fileHeader *multipart.FileHeader, client objectstorage.ObjectStorage) error {
	f, err := fileHeader.Open()
	if err != nil {
		return err
	}
	defer f.Close()

	if storageName == "sugon" || storageName == "starlight" {
		err := client.PutObjectWithTotalLength(ctx, bucketName, objectKey, dgst.String(), fileHeader.Size, f)
		if err != nil {
			return err
		}
	} else {
		err := client.PutObject(ctx, bucketName, objectKey, dgst.String(), f)
		if err != nil {
			return err
		}
	}
	return nil
}

// importObjectToSeedPeers uses to import object to local storage.
func (o *objectStorage) importObjectToLocalStorage(ctx context.Context, taskID, peerID string, fileHeader *multipart.FileHeader) error {
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
	tsd, err := o.storageManager.RegisterTask(ctx, &storage.RegisterTaskRequest{
		PeerTaskMetadata: meta,
	})
	if err != nil {
		return err
	}

	// Import task data to dfdaemon.
	if err := o.peerTaskManager.GetPieceManager().Import(ctx, meta, tsd, fileHeader.Size, f); err != nil {
		return err
	}

	return nil
}

// importObjectToSeedPeers uses to import object to available seed peers.
func (o *objectStorage) importObjectToSeedPeers(ctx context.Context, bucketName, objectKey, filter string, mode int, fileHeader *multipart.FileHeader, maxReplicas int, log *logger.SugaredLoggerOnWith) error {
	schedulers, err := o.dynconfig.GetSchedulers()
	if err != nil {
		return err
	}

	var seedPeerHosts []string
	for _, scheduler := range schedulers {
		for _, seedPeer := range scheduler.SeedPeers {
			if o.config.Host.AdvertiseIP.String() != seedPeer.Ip && seedPeer.ObjectStoragePort > 0 {
				seedPeerHosts = append(seedPeerHosts, fmt.Sprintf("%s:%d", seedPeer.Ip, seedPeer.ObjectStoragePort))
			}
		}
	}
	seedPeerHosts = pkgstrings.Unique(seedPeerHosts)

	var replicas int
	for _, seedPeerHost := range seedPeerHosts {
		log.Infof("import object %s to seed peer %s", objectKey, seedPeerHost)
		if err := o.importObjectToSeedPeer(ctx, seedPeerHost, bucketName, objectKey, filter, mode, fileHeader); err != nil {
			log.Errorf("import object %s to seed peer %s failed: %s", objectKey, seedPeerHost, err)
			continue
		}

		replicas++
		if replicas >= maxReplicas {
			break
		}
	}

	log.Infof("import %d object %s to seed peers", replicas, objectKey)
	return nil
}

// importObjectToSeedPeer uses to import object to seed peer.
func (o *objectStorage) importObjectToSeedPeer(ctx context.Context, seedPeerHost, bucketName, objectKey, filter string, mode int, fileHeader *multipart.FileHeader) error {
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
