package event

const (
	SystemEventJoin  = "join"
	SystemEventLeave = "leave"
)

type BroadcastSystemMessageRequest struct {
	Username string `json:"username"`
	Event    string `json:"event"`
}
