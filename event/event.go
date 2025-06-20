package event

import (
	"github.com/bluesky-social/indigo/api/atproto"
	"time"
)

// A JsonErrorFrame is the content of an error frame returned by any of the
// event streams.
type JsonErrorFrame struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// A JsonEvent is an event logged from any of the ATProto event streams, such
// as the Firehose or a Labeler's stream of labels.
//
// TODO add RepoSync once it's rolled out.
type JsonEvent struct {
	Received     time.Time                            `json:"received"`
	RepoCommit   *atproto.SyncSubscribeRepos_Commit   `json:"repo_commit,omitempty"`
	RepoIdentity *atproto.SyncSubscribeRepos_Identity `json:"repo_identity,omitempty"`
	RepoAccount  *atproto.SyncSubscribeRepos_Account  `json:"repo_account,omitempty"`
	RepoInfo     *atproto.SyncSubscribeRepos_Info     `json:"repo_info,omitempty"`
	LabelLabels  *atproto.LabelSubscribeLabels_Labels `json:"label_labels,omitempty"`
	LabelInfo    *atproto.LabelSubscribeLabels_Info   `json:"label_info,omitempty"`
	ErrorFrame   *JsonErrorFrame                      `json:"error_frame,omitempty"`
}
