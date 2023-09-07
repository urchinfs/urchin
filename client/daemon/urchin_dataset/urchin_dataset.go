package urchin_dataset

import (
	"crypto/md5"
	"d7y.io/dragonfly/v2/client/config"
	"d7y.io/dragonfly/v2/client/daemon/urchin_dataset_vesion"
	"d7y.io/dragonfly/v2/client/util"
	logger "d7y.io/dragonfly/v2/internal/dflog"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis"
	"github.com/google/uuid"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var conf *ConfInfo
var once sync.Once

type ConfInfo struct {
	Opt       *config.DaemonOption
	DynConfig config.Dynconfig
}

func SetDataSetConfInfo(opt *config.DaemonOption, dynConfig config.Dynconfig) {
	once.Do(func() {
		conf = &ConfInfo{
			Opt:       opt,
			DynConfig: dynConfig,
		}
	})
}

func getConfInfo() ConfInfo {
	return *conf
}

// CreateDataSet POST /api/v1/dataset
func CreateDataSet(ctx *gin.Context) {
	var form UrchinDataSetCreateParams
	if err := ctx.ShouldBind(&form); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		dataSetName   = form.Name
		dataSetDesc   = form.Desc
		replica       = form.Replica
		cacheStrategy = form.CacheStrategy
		dataSetTags   = form.Tags
	)

	dataSourcesInfo := getConfInfo().Opt.ObjectStorage

	if int(replica) > dataSourcesInfo.MaxReplicas {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": "replica: " + strconv.FormatUint(uint64(replica), 10) + " is large than the max datasource count of system setting: " + strconv.FormatInt(int64(dataSourcesInfo.MaxReplicas), 10)})
		return
	}

	schedulers, err := getConfInfo().DynConfig.GetSchedulers()
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	var replicableDataSourceCnt uint = 0
	for _, scheduler := range schedulers {
		for _, seedPeer := range scheduler.SeedPeers {
			if getConfInfo().Opt.Host.AdvertiseIP.String() != seedPeer.Ip && seedPeer.ObjectStoragePort > 0 {
				replicableDataSourceCnt++
			}
		}
	}
	if replica > replicableDataSourceCnt {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": "replica: " + strconv.FormatUint(uint64(replica), 10) + " is large than replicable datasource count: " + strconv.FormatUint(uint64(replicableDataSourceCnt), 10)})
		return
	}

	dataSetID := GetUUID()
	redisClient := util.NewRedisStorage(util.RedisClusterIP, util.RedisClusterPwd, false)
	datasetKey := redisClient.MakeStorageKey([]string{dataSetID}, StoragePrefixDataset)
	values := make(map[string]interface{})
	values["id"] = dataSetID
	values["name"] = dataSetName
	values["desc"] = dataSetDesc
	if replica <= 0 {
		values["replica"] = 1
	} else {
		values["replica"] = replica
	}
	values["cache_strategy"] = cacheStrategy
	values["tags"] = strings.Join(dataSetTags, "_")
	values["share_blob_sources"] = "[]"
	values["share_blob_caches"] = "[]"

	curTime := time.Now().Unix()
	values["create_time"] = strconv.FormatInt(curTime, 10)
	values["update_time"] = strconv.FormatInt(curTime, 10)
	err = redisClient.SetMapElements(datasetKey, values)
	if err != nil {
		logger.Warnf("CreateDataSet set map elements err:%v, dataSetID:%s", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	err = redisClient.ZAdd(DatasetCreateTimeKey, dataSetID, float64(curTime))
	if err != nil {
		logger.Warnf("CreateDataSet zadd element to que  err:%v, dataSetID:%s", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	if len(dataSetName) > 0 {
		datasetNameKey := redisClient.MakeStorageKey([]string{dataSetID, "match_prefix_name", dataSetName}, StoragePrefixDataset)
		err = redisClient.Set(datasetNameKey, []byte(dataSetName))
		if err != nil {
			logger.Warnf("CreateDataSet set dataset name err:%v, dataSetID:%s", err, dataSetID)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}
	}

	if len(dataSetTags) > 0 {
		formatTags := strings.Join(dataSetTags, "_")
		datasetTagsKey := redisClient.MakeStorageKey([]string{dataSetID, "match_prefix_tags", formatTags}, StoragePrefixDataset)
		err = redisClient.Set(datasetTagsKey, []byte(formatTags))
		if err != nil {
			logger.Warnf("CreateDataSet set dataset tags err:%v, dataSetID:%s", err, dataSetID)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}
	}

	_ = urchin_dataset_vesion.CreateDataSetVersionImpl(dataSetID, urchin_dataset_vesion.UrchinDataSetVersionInfo{
		ID:       DefaultDatasetVersion,
		Name:     "default dataset version",
		CreateAt: curTime,
	})

	ctx.JSON(http.StatusOK, gin.H{
		"status_code": 0,
		"status_msg":  "succeed",
		"dataset_id":  dataSetID,
	})
	return
}

// UpdateDataSet PATCH /api/v1/dataset/:datasetid
func UpdateDataSet(ctx *gin.Context) {
	var params UrchinDataSetParams
	if err := ctx.ShouldBindUri(&params); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var form UrchinDataSetUpdateParams
	if err := ctx.ShouldBind(&form); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		dataSetID     = params.ID
		dataSetDesc   = form.Desc
		replica       = form.Replica
		cacheStrategy = form.CacheStrategy
		dataSetTags   = form.Tags
	)

	err := UpdateDataSetImpl(dataSetID, dataSetDesc, replica, cacheStrategy, dataSetTags, []UrchinEndpoint{}, []UrchinEndpoint{})
	if err != nil {
		logger.Warnf("UpdateDataSet err:%v, dataSetID:%s, dataSetDesc:%s", err, dataSetID, dataSetDesc)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	ctx.JSON(http.StatusOK, gin.H{
		"status_code": 0,
		"status_msg":  "succeed",
	})
	return
}

// GetDataSet GET /api/v1/dataset/:datasetid
func GetDataSet(ctx *gin.Context) {
	var params UrchinDataSetParams
	if err := ctx.ShouldBindUri(&params); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		dataSetID = params.ID
	)

	dataset, err := GetDataSetImpl(dataSetID)
	if err != nil {
		logger.Warnf("GetDataSet fail, err:%v, dataSetID:%s", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	ctx.JSON(http.StatusOK, gin.H{
		"status_code": 0,
		"status_msg":  "succeed",
		"dataset":     dataset,
	})
	return
}

// ListDataSets GET /api/v1/datasets
func ListDataSets(ctx *gin.Context) {
	var form UrchinDataSetQueryParams
	if err := ctx.ShouldBind(&form); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		pageIndex        = form.PageIndex
		pageSize         = form.PageSize
		searchKey        = form.SearchKey
		orderBy          = form.OrderBy
		sortBy           = form.SortBy
		createdAtLess    = form.CreatedAtLess
		createdAtGreater = form.CreatedAtGreater
	)

	getCacheSortSet := func() string {
		formPara := searchKey + orderBy + fmt.Sprint(sortBy) + fmt.Sprint(createdAtLess) + fmt.Sprint(createdAtGreater)
		h := md5.New()
		h.Write([]byte(formPara))

		curTime := time.Now()
		return DataSetTmpSortSet + "_" + hex.EncodeToString(h.Sum(nil)) + "_" + fmt.Sprint(curTime.Unix()-int64(curTime.Second()%20))
	}

	var datasets []UrchinDataSetInfo
	redisClient := util.NewRedisStorage(util.RedisClusterIP, util.RedisClusterPwd, false)
	if searchKey == "" {
		if orderBy == "" {
			var rangeLower, rangeUpper int64
			if createdAtLess != 0 {
				rangeUpper = createdAtLess
			} else {
				rangeUpper = time.Now().Unix() + 1
			}
			if createdAtGreater != 0 {
				rangeLower = createdAtGreater
			} else {
				rangeLower = 0
			}

			var members []string
			var err error
			if sortBy == 1 {
				members, err = redisClient.ZRangeByScore(DatasetCreateTimeKey, strconv.FormatInt(rangeLower, 10), strconv.FormatInt(rangeUpper, 10), int64(pageIndex), int64(pageSize))
			} else {
				members, err = redisClient.ZRevRangeByScore(DatasetCreateTimeKey, strconv.FormatInt(rangeLower, 10), strconv.FormatInt(rangeUpper, 10), int64(pageIndex), int64(pageSize))
			}
			if err != nil {
				logger.Warnf("ListDataSets range by score err:%v", err)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}

			for _, member := range members {
				dataset, err := getDataSetById(member, redisClient)
				if err != nil {
					logger.Warnf("ListDataSets get dataset by id err:%v", err)
					ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
					return
				}

				datasets = append(datasets, dataset)
			}
		} else {
			var tmpSortSetKey string
			if createdAtLess != 0 || createdAtGreater != 0 {
				tmpSortSetKey = redisClient.MakeStorageKey([]string{getCacheSortSet()}, StoragePrefixDataset)
				exists, err := redisClient.Exists(tmpSortSetKey)
				if err != nil || !exists {
					var members []string
					err := MatchZSetMemberByCreateTime(createdAtLess, createdAtGreater, DatasetCreateTimeKey, &members, redisClient)
					if err != nil {
						logger.Warnf("ListDataSets match dataset by create time err:%v", err)
						ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
						return
					}

					tmpSortSetKey = redisClient.MakeStorageKey([]string{getCacheSortSet()}, StoragePrefixDataset)
					err = WriteToTmpSet(members, tmpSortSetKey, redisClient)
					if err != nil {
						logger.Warnf("ListDataSets write to tmp set err:%v", err)
						ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
						return
					}
				}

			} else {
				tmpSortSetKey = DatasetCreateTimeKey
			}

			err := sortAndBuildResult(orderBy, sortBy, pageIndex, pageSize, tmpSortSetKey, redisClient, &datasets)
			if err != nil {
				logger.Warnf("ListDataSets sort and build result err:%v", err)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}
		}

	} else {
		var tmpSortSetKey string

		tmpSortSetKey = redisClient.MakeStorageKey([]string{getCacheSortSet()}, StoragePrefixDataset)
		exists, err := redisClient.Exists(tmpSortSetKey)
		if err != nil || !exists {
			matchName := make(map[string]bool)
			prefix := StoragePrefixDataset + "*" + "match_prefix_name:*" + searchKey + "*"
			err := MatchKeysByPrefix(prefix, matchName, redisClient)
			if err != nil {
				logger.Warnf("ListDataSets match dataset by name prefix err:%v, prefix:%s", err, prefix)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}

			matchTags := make(map[string]bool)
			prefix = StoragePrefixDataset + "*" + "match_prefix_tags:*" + searchKey + "*"
			err = MatchKeysByPrefix(prefix, matchTags, redisClient)
			if err != nil {
				logger.Warnf("ListDataSets match dataset by tags prefix err:%v, prefix:%s", err, prefix)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}

			var matchResult []string
			if createdAtLess != 0 || createdAtGreater != 0 {
				err = MatchZSetMemberByCreateTime(createdAtLess, createdAtGreater, DatasetCreateTimeKey, &matchResult, redisClient)
				if err != nil {
					logger.Warnf("ListDataSets match dataset by create time err:%v", err)
					ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
					return
				}
			}

			matchCreateTime := make(map[string]bool)
			for _, member := range matchResult {
				matchCreateTime[member] = true
			}

			matchMap := unionMap(matchName, matchTags)
			if createdAtLess != 0 || createdAtGreater != 0 {
				matchMap = InterMap(matchMap, matchCreateTime)
			}

			matchSlice := MapToSlice(matchMap)
			tmpSortSetKey = redisClient.MakeStorageKey([]string{getCacheSortSet()}, StoragePrefixDataset)
			err = WriteToTmpSet(matchSlice, tmpSortSetKey, redisClient)
			if err != nil {
				logger.Warnf("ListDataSets write to tmp set err:%v", err)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}
		}

		err = sortAndBuildResult(orderBy, sortBy, pageIndex, pageSize, tmpSortSetKey, redisClient, &datasets)
		if err != nil {
			logger.Warnf("ListDataSets sort and build result err:%v", err)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}
	}

	ctx.JSON(http.StatusOK, gin.H{
		"status_code": 0,
		"status_msg":  "succeed",
		"datasets":    datasets,
	})
	return
}

// DeleteDataSet DELETE /api/v1/dataset/:datasetid
func DeleteDataSet(ctx *gin.Context) {
	var params UrchinDataSetParams
	if err := ctx.ShouldBindUri(&params); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		dataSetID = params.ID
	)

	redisClient := util.NewRedisStorage(util.RedisClusterIP, util.RedisClusterPwd, false)
	datasetKey := redisClient.MakeStorageKey([]string{dataSetID}, StoragePrefixDataset)

	dataSetName, err := redisClient.GetMapElement(datasetKey, "name")
	if err != nil {
		logger.Warnf("DeleteDataSet get map element name err:%v, dataSetID:%s", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	if len(dataSetName) > 0 {
		datasetNameKey := redisClient.MakeStorageKey([]string{dataSetID, "match_prefix_name", dataSetName}, StoragePrefixDataset)
		err := redisClient.Del(datasetNameKey)
		if err != nil {
			logger.Warnf("DeleteDataSet del key %s err:%v, dataSetID:%s", datasetNameKey, err)
		}
	}

	dataSetTags, err := redisClient.GetMapElement(datasetKey, "tags")
	if err != nil {
		logger.Warnf("DeleteDataSet get map element tags err:%v", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	if len(dataSetTags) > 0 {
		datasetTagsKey := redisClient.MakeStorageKey([]string{dataSetID, "match_prefix_tags", dataSetTags}, StoragePrefixDataset)
		err := redisClient.Del(datasetTagsKey)
		if err != nil {
			logger.Warnf("DeleteDataSet del key %s err:%v", datasetTagsKey, err)
		}
	}

	err = redisClient.ZRem(DatasetCreateTimeKey, dataSetID)
	if err != nil {
		logger.Warnf("DeleteDataSet zRem key %s err:%v", dataSetID, err)
	}

	err = redisClient.DeleteMap(datasetKey)
	if err != nil {
		logger.Warnf("DeleteDataSet del map err:%v, dataSetID:%s", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}
	err = redisClient.Del(datasetKey)
	if err != nil {
		logger.Warnf("DeleteDataSet del map key err:%v, dataSetID:%s", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	ctx.JSON(http.StatusOK, gin.H{
		"status_code": 0,
		"status_msg":  "succeed",
	})
	return
}

func unionMap(m1, m2 map[string]bool) map[string]bool {
	result := make(map[string]bool)
	for k, v := range m1 {
		result[k] = v
	}
	for k, v := range m2 {
		if _, ok := result[k]; !ok {
			result[k] = v
		}
	}
	return result
}

func InterMap(m1, m2 map[string]bool) map[string]bool {
	result := make(map[string]bool)
	for k, v := range m1 {
		if _, ok := m2[k]; ok {
			result[k] = v
		}
	}
	return result
}

func MapToSlice(m map[string]bool) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	return s
}

func UpdateDataSetImpl(dataSetID, dataSetDesc string, replica uint, cacheStrategy string, dataSetTags []string,
	shareBlobSources, shareBlobCaches []UrchinEndpoint) error {
	logger.Infof("updateDataSet dataSetID:%s, desc:%s replica:%d cacheStrategy:%s tags:%v shareBlobSources:%v shareBlobCaches:%v",
		dataSetID, dataSetDesc, replica, cacheStrategy, dataSetTags, shareBlobSources, shareBlobCaches)

	_, err := GetDataSetImpl(dataSetID)
	if err != nil {
		logger.Warnf("updateDataSet get dataSet err:%v, dataSetID:%s", err, dataSetID)
		return err
	}

	redisClient := util.NewRedisStorage(util.RedisClusterIP, util.RedisClusterPwd, false)
	datasetKey := redisClient.MakeStorageKey([]string{dataSetID}, StoragePrefixDataset)
	if len(dataSetDesc) > 0 {
		err := redisClient.SetMapElement(datasetKey, "desc", []byte(dataSetDesc))
		if err != nil {
			logger.Warnf("updateDataSet set map element err:%v, dataSetID:%s, desc:%s", err, dataSetID, dataSetDesc)
			return err
		}
	}
	if replica > 0 {
		err := redisClient.SetMapElement(datasetKey, "replica", []byte(strconv.FormatInt(int64(replica), 10)))
		if err != nil {
			logger.Warnf("updateDataSet set map element err:%v, dataSetID:%s, replica:%d", err, dataSetID, replica)
			return err
		}
	}
	if len(cacheStrategy) > 0 {
		err := redisClient.SetMapElement(datasetKey, "cache_strategy", []byte(cacheStrategy))
		if err != nil {
			logger.Warnf("updateDataSet set map element err:%v, dataSetID:%s, cache_strategy:%d", err, dataSetID, cacheStrategy)
			return err
		}
	}
	if len(dataSetTags) > 0 {
		oldTags, err := redisClient.GetMapElement(datasetKey, "tags")
		if err != nil {
			logger.Warnf("updateDataSet get map old element err:%v, dataSetID:%s, tags:%d", err, dataSetID, dataSetTags)
			return err
		}

		oldTagsKey := redisClient.MakeStorageKey([]string{dataSetID, "match_prefix_tags", oldTags}, StoragePrefixDataset)
		_ = redisClient.Del(oldTagsKey)

		err = redisClient.SetMapElement(datasetKey, "tags", []byte(strings.Join(dataSetTags, "_")))
		if err != nil {
			logger.Warnf("updateDataSet set map element err:%v, dataSetID:%s, tags:%d", err, dataSetID, dataSetTags)
			return err
		}

		formatTags := strings.Join(dataSetTags, "_")
		datasetTagsKey := redisClient.MakeStorageKey([]string{dataSetID, "match_prefix_tags", formatTags}, StoragePrefixDataset)
		err = redisClient.Set(datasetTagsKey, []byte(formatTags))
		if err != nil {
			logger.Warnf("updateDataSet set dataset tags err:%v, dataSetID:%s", err, dataSetID)
			return err
		}
	}

	if len(shareBlobSources) > 0 {
		jsonBody, err := json.Marshal(shareBlobSources)
		if err != nil {
			logger.Warnf("updateDataSet json marshal err:%v, dataSetID:%s, shareBlobSources:%d", err, dataSetID, shareBlobSources)
			return err
		}
		err = redisClient.SetMapElement(datasetKey, "share_blob_sources", jsonBody)
		if err != nil {
			logger.Warnf("updateDataSet set map element err:%v, dataSetID:%s, shareBlobSources:%d", err, dataSetID, shareBlobSources)
			return err
		}
	}

	if len(shareBlobCaches) > 0 {
		jsonBody, err := json.Marshal(shareBlobCaches)
		if err != nil {
			logger.Warnf("updateDataSet json marshal err:%v, dataSetID:%s, shareBlobCaches:%d", err, dataSetID, shareBlobCaches)
			return err
		}
		err = redisClient.SetMapElement(datasetKey, "share_blob_caches", jsonBody)
		if err != nil {
			logger.Warnf("updateDataSet set map element err:%v, dataSetID:%s, shareBlobCaches:%d", err, dataSetID, shareBlobCaches)
			return err
		}
	}

	curTime := time.Now().Unix()
	_ = redisClient.SetMapElement(datasetKey, "update_time", []byte(strconv.FormatInt(curTime, 10)))

	logger.Infof("updateDataSet dataSetID:%s complete", dataSetID)
	return nil
}

func GetDataSetImpl(dataSetID string) (UrchinDataSetInfo, error) {
	if dataSetID == "" {
		return UrchinDataSetInfo{}, fmt.Errorf("dataSet ID is empty")
	}

	redisClient := util.NewRedisStorage(util.RedisClusterIP, util.RedisClusterPwd, false)
	datasetKey := redisClient.MakeStorageKey([]string{dataSetID}, StoragePrefixDataset)
	elements, err := redisClient.ReadMap(datasetKey)
	if err != nil {
		logger.Warnf("GetDataSetImpl read map element err:%v, dataSetID:%s", err, dataSetID)
		return UrchinDataSetInfo{}, err
	}

	if string(elements["id"]) != dataSetID {
		logger.Warnf("GetDataSetImpl can not found dataSetID:%s", dataSetID)
		return UrchinDataSetInfo{}, err
	}

	err = nil
	var dataset UrchinDataSetInfo
	for k, v := range elements {
		if k == "tags" {
			dataset.Tags = strings.Split(string(v), "_")
		} else if k == "share_blob_sources" {
			err = json.Unmarshal(v, &dataset.ShareBlobSources)
		} else if k == "share_blob_caches" {
			err = json.Unmarshal(v, &dataset.ShareBlobCaches)
		} else if k == "id" {
			dataset.Id = string(v)
		} else if k == "name" {
			dataset.Name = string(v)
		} else if k == "desc" {
			dataset.Desc = string(v)
		} else if k == "replica" {
			var tmpReplica int
			tmpReplica, err = strconv.Atoi(string(v))
			dataset.Replica = uint(tmpReplica)
		} else if k == "cache_strategy" {
			dataset.CacheStrategy = string(v)
		}

		if err != nil {
			logger.Warnf("GetDataSetImpl json unmarshal err:%v, dataSetID:%s", err, dataSetID)
			return UrchinDataSetInfo{}, err
		}
	}

	return dataset, nil
}

func getDataSetById(dataSetID string, redisClient *util.RedisStorage) (UrchinDataSetInfo, error) {
	var dataset UrchinDataSetInfo
	datasetKey := redisClient.MakeStorageKey([]string{dataSetID}, StoragePrefixDataset)
	elements, err := redisClient.ReadMap(datasetKey)
	if err != nil {
		logger.Warnf("getDataSetById read map element err:%v, dataSetID:%s", err, dataSetID)
		return dataset, err
	}

	for k, v := range elements {
		if k == "tags" {
			dataset.Tags = strings.Split(string(v), "_")
		} else if k == "share_blob_sources" {
			err = json.Unmarshal(v, &dataset.ShareBlobSources)
		} else if k == "share_blob_caches" {
			err = json.Unmarshal(v, &dataset.ShareBlobCaches)
		} else if k == "id" {
			dataset.Id = string(v)
		} else if k == "name" {
			dataset.Name = string(v)
		} else if k == "desc" {
			dataset.Desc = string(v)
		} else if k == "replica" {
			var tmpReplica int
			tmpReplica, err = strconv.Atoi(string(v))
			dataset.Replica = uint(tmpReplica)
		} else if k == "cache_strategy" {
			dataset.CacheStrategy = string(v)
		}

		if err != nil {
			logger.Warnf("getDataSetById json unmarshal err:%v, dataSetID:%s", err, dataSetID)
			return dataset, err
		}
	}

	return dataset, nil
}

func WriteToTmpSet(members []string, tmpSortSetKey string, redisClient *util.RedisStorage) error {
	for _, member := range members {
		err := redisClient.InsertSet(tmpSortSetKey, member)
		if err != nil {
			return err
		}
	}

	_ = redisClient.SetTTL(tmpSortSetKey, time.Second*120)
	return nil
}

func MatchKeysByPrefix(prefix string, matchResult map[string]bool, redisClient *util.RedisStorage) error {
	var cursor uint64
	for {
		members, cursor, err := redisClient.Scan(cursor, prefix, 100)
		if err != nil {
			return err
		}

		for _, member := range members {
			segments := strings.Split(member, ":")
			matchResult[segments[2]] = true
		}

		if cursor == 0 {
			break
		}
	}

	return nil
}

func MatchZSetMemberByCreateTime(createdAtLess, createdAtGreater int64, zsetKey string, matchResult *[]string, redisClient *util.RedisStorage) error {
	var rangeLower, rangeUpper int64
	if createdAtLess != 0 {
		rangeUpper = createdAtLess
	} else {
		rangeUpper = time.Now().Unix() + 1
	}
	if createdAtGreater != 0 {
		rangeLower = createdAtGreater
	} else {
		rangeLower = 0
	}

	var offset int64 = 0
	var count int64 = 100
	for {
		members, err := redisClient.ZRangeByScore(zsetKey, strconv.FormatInt(rangeLower, 10), strconv.FormatInt(rangeUpper, 10), offset, count)
		if err != nil {
			return err
		}

		*matchResult = append(*matchResult, members...)

		if len(members) <= 0 {
			break
		}

		offset += count
	}

	return nil
}

func sortAndBuildResult(orderBy string, sortBy int, pageIndex, pageSize int, sortSetKey string, redisClient *util.RedisStorage, datasets *[]UrchinDataSetInfo) error {
	if orderBy == "" {
		orderBy = "create_time"
	}

	sortByStr := "ASC"
	if sortBy == -1 {
		sortByStr = "DESC"
	}

	sortByPara := StoragePrefixDataset + ":" + "*->" + orderBy
	sort := redis.Sort{
		By:     sortByPara,
		Offset: int64(pageIndex),
		Count:  int64(pageSize),
		Order:  sortByStr,
		Alpha:  true,
	}
	members, err := redisClient.Sort(sortSetKey, sort)
	if err != nil {
		logger.Warnf("ListDataSets->sortAndBuildResult sort err:%v", err)
		return err
	}

	for _, member := range members {
		dataset, err := getDataSetById(member, redisClient)
		if err != nil {
			logger.Warnf("ListDataSets->sortAndBuildResult get dataset by id err:%v", err)
			continue
		}

		if dataset.Id == "" {
			continue
		}

		*datasets = append(*datasets, dataset)
	}

	return nil
}

func GetUUID() string {
	u2 := uuid.New()
	return u2.String()
}
