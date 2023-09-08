package urchin_dataset

const (
	StoragePrefix         = "urchin"
	StoragePrefixDataset  = "urchin:dataset"
	DatasetCreateTimeKey  = "urchin:dataset:create_time_que"
	DataSetTmpSortSet     = "tmpSortDatasetSet"
	DefaultDatasetVersion = "default"
)

type UrchinDataSetParams struct {
	ID string `uri:"dataset_id" binding:"required"`
}

type UrchinDataSetCreateParams struct {
	Name          string   `form:"name" binding:"required"`
	Desc          string   `form:"desc" binding:"omitempty"`
	Replica       uint     `form:"replica,default=1" binding:"omitempty"`
	CacheStrategy string   `form:"cache_strategy=local_storage" binding:"omitempty"`
	Tags          []string `form:"tags" binding:"omitempty"`
}

type UrchinDataSetUpdateParams struct {
	Name          string   `form:"name" binding:"omitempty"`
	Desc          string   `form:"desc" binding:"omitempty"`
	Replica       uint     `form:"replica" binding:"omitempty"`
	CacheStrategy string   `form:"cache_strategy" binding:"omitempty"`
	Tags          []string `form:"tags" binding:"omitempty"`
}

type UrchinDataSetQueryParams struct {
	PageIndex        int    `form:"page_index,default=0" binding:"omitempty"`
	PageSize         int    `form:"page_size,default=100" binding:"omitempty"`
	SearchKey        string `form:"search_key" binding:"omitempty"`
	OrderBy          string `form:"order_by" binding:"omitempty"`
	SortBy           int    `form:"sort_by,default=1" binding:"omitempty"`
	CreatedAtLess    int64  `form:"created_at_less,default=0" binding:"omitempty"`
	CreatedAtGreater int64  `form:"created_at_greater,default=0" binding:"omitempty"`
}

type UrchinEndpoint struct {
	Endpoint     string `json:"endpoint"`
	EndpointPath string `json:"endpoint_path"`
}

type UrchinDataSetInfo struct {
	Id               string           `json:"id"`
	Name             string           `json:"name"`
	Desc             string           `json:"desc"`
	Replica          uint             `json:"replica"`
	CacheStrategy    string           `json:"cache_strategy"`
	Tags             []string         `json:"tags"`
	ShareBlobSources []UrchinEndpoint `json:"share_blob_sources"`
	ShareBlobCaches  []UrchinEndpoint `json:"share_blob_caches"`
}
