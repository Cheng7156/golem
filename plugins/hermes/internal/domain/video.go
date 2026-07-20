package domain

type VideoOutput struct {
	ObjectID      string `json:"object_id"`
	ThumbObjectID string `json:"thumb_object_id"`
	Duration      uint32 `json:"duration"`
	Title         string `json:"title,omitempty"`
	PageURL       string `json:"page_url,omitempty"`
}
