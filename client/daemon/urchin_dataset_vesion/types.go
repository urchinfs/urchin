package urchin_dataset_vesion

type UrchinDataSetVersionCreateUriParams struct {
	DataSetID string `uri:"dataset_id" binding:"required"`
}

type UrchinDataSetVersionCreateParams struct {
	VersionID string `form:"version_id" json:"version_id"`
}

type UrchinDataSetVersionUriParams struct {
	DataSetID string `uri:"dataset_id" binding:"required"`
	VersionID string `uri:"version_id" binding:"required"`
}

type UrchinDataSetVersionInfo struct {
	ID          string `form:"id" json:"id"`
	Name        string `form:"name" json:"name"`
	MetaSources string `form:"meta_sources" json:"meta_sources"`
	MetaCaches  string `form:"meta_caches" json:"meta_caches"`
	CreateAt    int64  `form:"create_at" json:"create_at"`
}

type UrchinDataSetVersionListParams struct {
	PageIndex       int    `form:"page_index,default=0" binding:"omitempty,gte=0" json:"page_index"`
	PageSize        int    `form:"page_size,default=1" binding:"omitempty,gte=1" json:"page_size"`
	SearchKey       string `form:"search_key" json:"search_key"`
	OrderBy         string `form:"order_by" json:"order_by"`
	SortBy          int    `form:"sort_by" json:"sort_by"`
	CreateAtLess    int64  `form:"create_at_less" json:"create_at_less"`
	CreateAtGreater int64  `form:"create_at_greater" json:"create_at_greater"`
}

const (
	datasetversionPrefix = "urchin:datasetversions:"
)
