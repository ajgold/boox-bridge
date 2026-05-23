package dto

import "github.com/sysop/ultrabridge/internal/spcserver/envelope"

// EntriesVO is the Dropbox-style file/folder entry the device reads in
// list_folder and the single-entry query VOs. Field names are verbatim from
// com/ratta/file/vo/EntriesVO.java (no @JsonProperty overrides — Jackson
// serializes the Java field names as-is, so the snake_case names below are the
// real wire keys). Tag is "file" or "folder" (EntriesVO.java:16 "文件夹或者文件标志").
// ContentHash carries the file MD5 (EntriesVO.java:24 "文件md5").
type EntriesVO struct {
	Tag            string `json:"tag"`
	ID             string `json:"id"`
	Name           string `json:"name"`
	PathDisplay    string `json:"path_display"`
	ContentHash    string `json:"content_hash"`
	IsDownloadable bool   `json:"is_downloadable"`
	Size           int64  `json:"size"`
	LastUpdateTime int64  `json:"lastUpdateTime"`
	ParentPath     string `json:"parent_path"`
}

// CapacityVO is the response to POST /api/file/capacity/query (the variant the
// device hits in 0b). Extends BaseVO (com/ratta/file/vo/CapacityVO.java) so
// success/usedCapacity/totalCapacity serialize flat.
type CapacityVO struct {
	envelope.BaseVO
	UsedCapacity  int64 `json:"usedCapacity"`
	TotalCapacity int64 `json:"totalCapacity"`
}

// CapacityLocalVO is the response to POST /api/file/2/users/get_space_usage.
// Extends BaseVO (com/ratta/file/vo/CapacityLocalVO.java); allocationVO is a
// nested object (AllocationVO does NOT extend BaseVO).
type CapacityLocalVO struct {
	envelope.BaseVO
	Used         int64        `json:"used"`
	AllocationVO AllocationVO `json:"allocationVO"`
	EquipmentNo  string       `json:"equipmentNo"`
}

// AllocationVO is the nested quota descriptor inside CapacityLocalVO
// (com/ratta/file/vo/AllocationVO.java: tag, allocated).
type AllocationVO struct {
	Tag       string `json:"tag"`
	Allocated int64  `json:"allocated"`
}
