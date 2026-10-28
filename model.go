// Package resources owns learning-resource publications and configurable taxonomy.
// Files, academic references, moderation and entitlements are supplied through ports.
package resources

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"time"
)

var (
	ErrInput           = errors.New("resources_invalid_input")
	ErrUnauthenticated = errors.New("resources_authentication_required")
	ErrDenied          = errors.New("resources_permission_denied")
	ErrNotFound        = errors.New("resources_not_found")
	ErrConflict        = errors.New("resources_conflict")
	ErrUnavailable     = errors.New("resources_unavailable")
	ErrCapacity        = errors.New("resources_capacity_exceeded")
)

type Principal struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

func (p Principal) Valid() bool {
	return p.Issuer != "" && p.Subject != "" && len(p.Issuer) <= 512 && len(p.Subject) <= 256
}
func (p Principal) Key() string {
	if !p.Valid() {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(p.Issuer)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(p.Subject))
}

type Names map[string]string
type Value struct {
	Text   *string `json:"text,omitempty"`
	Number *int64  `json:"number,omitempty"`
	Option *string `json:"option,omitempty"`
}
type Option struct {
	ID    string `json:"id"`
	Names Names  `json:"names"`
}
type Field struct {
	Key        string   `json:"key"`
	Names      Names    `json:"names"`
	Kind       string   `json:"kind"`
	Required   bool     `json:"required"`
	Filterable bool     `json:"filterable"`
	Groupable  bool     `json:"groupable"`
	Min        *int64   `json:"min,omitempty"`
	Max        *int64   `json:"max,omitempty"`
	MaxLength  int      `json:"max_length,omitempty"`
	Options    []Option `json:"options,omitempty"`
}
type Category struct {
	ID       string  `json:"id"`
	Revision int64   `json:"revision"`
	Names    Names   `json:"names"`
	Enabled  bool    `json:"enabled"`
	Order    int     `json:"order"`
	Fields   []Field `json:"fields"`
	GroupBy  string  `json:"group_by"`
}
type CategoryWrite struct {
	Revision int64   `json:"revision"`
	Names    Names   `json:"names"`
	Enabled  bool    `json:"enabled"`
	Order    int     `json:"order"`
	Fields   []Field `json:"fields"`
	GroupBy  string  `json:"group_by"`
}
type CategoryPage struct {
	Items      []Category `json:"items"`
	NextCursor string     `json:"next_cursor"`
}
type Associations struct {
	SchoolID   string `json:"school_id"`
	CourseID   string `json:"course_id,omitempty"`
	TermID     string `json:"term_id,omitempty"`
	OfferingID string `json:"offering_id,omitempty"`
	TeacherID  string `json:"teacher_id,omitempty"`
}
type Consent struct {
	Accepted bool   `json:"accepted"`
	Version  string `json:"version"`
	Digest   string `json:"digest"`
}
type Origin struct {
	Kind    string   `json:"kind"`
	Source  string   `json:"source,omitempty"`
	Author  string   `json:"author,omitempty"`
	License string   `json:"license,omitempty"`
	Consent *Consent `json:"consent,omitempty"`
}
type Metadata struct {
	Title            string           `json:"title"`
	Description      string           `json:"description"`
	Tags             []string         `json:"tags"`
	CategoryID       string           `json:"category_id"`
	CategoryRevision int64            `json:"category_revision"`
	Attributes       map[string]Value `json:"attributes"`
	Associations     Associations     `json:"associations"`
	Access           string           `json:"access"`
	Origin           Origin           `json:"origin"`
	DirectoryID      string           `json:"directory_id,omitempty"`
}
type DraftWrite struct {
	Revision int64    `json:"revision"`
	Metadata Metadata `json:"metadata"`
}
type FileLink struct {
	Revision       int64  `json:"revision"`
	MediaVersionID string `json:"media_version_id"`
}
type Command struct {
	Revision int64 `json:"revision"`
}
type File struct {
	VersionID   string `json:"version_id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}
type Version struct {
	ID        string    `json:"id"`
	Number    int64     `json:"number"`
	Metadata  Metadata  `json:"metadata"`
	Category  Category  `json:"category"`
	File      *File     `json:"file,omitempty"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

// Record and VersionRecord are private repository/port models, never HTTP DTOs.
type Record struct {
	ID          string
	Owner       Principal
	SchoolID    string
	Revision    int64
	State       string
	ActiveID    string
	ProposalID  string
	NextVersion int64
	CreatedAt   time.Time
}
type VersionRecord struct {
	Version          Version
	ResourceID       string
	FileOwner        Principal
	ReferenceID      string
	ModerationTaskID string
	DecisionID       string
	DecisionRevision int64
}
type Resource struct {
	ID              string   `json:"id"`
	Revision        int64    `json:"revision"`
	State           string   `json:"state"`
	Current         *Version `json:"current,omitempty"`
	Proposal        *Version `json:"proposal,omitempty"`
	DownloadAllowed bool     `json:"download_allowed"`
	AccessReason    string   `json:"access_reason"`
	Owned           bool     `json:"owned"`
}
type Page struct {
	Items      []Resource `json:"items"`
	NextCursor string     `json:"next_cursor"`
}
type Filter struct {
	SchoolID, CourseID, CategoryID, DirectoryID, Query, After, OwnerKey string
	Attributes                                                          map[string]Value
	Limit                                                               int
	IncludeUnpublished                                                  bool
}
type Decision struct {
	ID        string `json:"id"`
	Revision  int64  `json:"revision"`
	VersionID string `json:"version_id"`
	Action    string `json:"action"`
	Reason    string `json:"reason"`
}
type StateChange struct {
	Revision int64  `json:"revision"`
	State    string `json:"state"`
	Reason   string `json:"reason"`
}
type Request struct {
	PreviousTaskID string   `json:"previous_task_id,omitempty"`
	Kind           string   `json:"kind"`
	Reason         string   `json:"reason"`
	Contact        string   `json:"contact,omitempty"`
	Evidence       []string `json:"evidence,omitempty"`
}
type RequestReceipt struct {
	TaskID         string    `json:"task_id,omitempty"`
	ResourceID     string    `json:"resource_id,omitempty"`
	Kind           string    `json:"kind,omitempty"`
	DecisionReason string    `json:"decision_reason,omitempty"`
	CanAppeal      bool      `json:"can_appeal"`
	ID             string    `json:"id"`
	State          string    `json:"state"`
	CreatedAt      time.Time `json:"created_at"`
}
type RequestRecord struct {
	Receipt    RequestReceipt
	ResourceID string
	VersionID  string
	Reporter   Principal
	Request    Request
	ExternalID string
}
type Grant struct {
	SubjectKey string     `json:"subject_key"`
	ID         string     `json:"id"`
	Revision   int64      `json:"revision"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Revoked    bool       `json:"revoked"`
}
type GrantWrite struct {
	Revision   int64      `json:"revision"`
	SubjectKey string     `json:"subject_key"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Revoked    bool       `json:"revoked"`
}
type GrantRecord struct {
	Grant                  Grant
	ResourceID, SubjectKey string
}
type GrantPage struct {
	Items      []Grant `json:"items"`
	NextCursor string  `json:"next_cursor"`
}
type Directory struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	SchoolID string `json:"school_id"`
	CourseID string `json:"course_id,omitempty"`
	ParentID string `json:"parent_id,omitempty"`
	Names    Names  `json:"names"`
	Enabled  bool   `json:"enabled"`
}
type DirectoryWrite struct {
	Revision int64  `json:"revision"`
	SchoolID string `json:"school_id"`
	CourseID string `json:"course_id,omitempty"`
	ParentID string `json:"parent_id,omitempty"`
	Names    Names  `json:"names"`
	Enabled  bool   `json:"enabled"`
}
type DirectoryPage struct {
	Items      []Directory `json:"items"`
	NextCursor string      `json:"next_cursor"`
}
type PackageRequest struct {
	ResourceIDs []string `json:"resource_ids,omitempty"`
	DirectoryID string   `json:"directory_id,omitempty"`
}
type PackagePart struct {
	Number int    `json:"number"`
	Files  []File `json:"files"`
	Bytes  int64  `json:"bytes"`
}
type Package struct {
	ID        string        `json:"id"`
	Parts     []PackagePart `json:"parts"`
	ExpiresAt time.Time     `json:"expires_at"`
}
type Selection struct {
	ResourceID, VersionID, ReferenceID string
	File                               File
}
type PackageRecord struct {
	Package    Package
	Owner      Principal
	Selections [][]Selection
}
type Terms struct {
	Version string `json:"version"`
	Digest  string `json:"digest"`
	URL     string `json:"url"`
}
type Capabilities struct {
	UploadProfiles    []UploadProfile `json:"upload_profiles"`
	Terms             Terms           `json:"terms"`
	AllowedTypes      []string        `json:"allowed_types"`
	MaxFileSize       int64           `json:"max_file_size"`
	MaxPackageBytes   int64           `json:"max_package_bytes"`
	MaxPackageFiles   int             `json:"max_package_files"`
	PurchaseAvailable bool            `json:"purchase_available"`
}

// Authority supplies current action eligibility and administration scope. Upload
// and download decisions include the configured campus/phone facts. Missing facts
// return unavailable; a permission grant cannot silently stand in for a fact.
type Authority interface {
	Check(context.Context, Principal, string, string) error
}
type Catalog interface {
	Validate(context.Context, Associations) error
}
type FileEvidence struct {
	File                            File
	Owner                           Principal
	Module, State, Access, SchoolID string
}
type Publication struct {
	ResourceID, VersionID, MediaVersionID, OperationID, Access string
	Owner                                                      Principal
}
type Files interface {
	Inspect(context.Context, Principal, string) (FileEvidence, error)
	Publish(context.Context, Publication) (reference string, ready bool, err error)
	Release(context.Context, string, string) error
	Download(context.Context, Principal, string, io.Writer) error
	Archive(context.Context, Principal, []Selection, io.Writer) error
}
type ReviewSubmission struct {
	ResourceID, VersionID, SchoolID, Title, Description string
	Revision                                            int64
}
type Moderation interface {
	Status(context.Context, Principal, string) (RequestStatus, error)
	Submit(context.Context, string, ReviewSubmission) (string, error)
	Request(context.Context, string, string, Principal, Request) (string, error)
}
type RequestStatus struct {
	State, Reason string
	CanAppeal     bool
}
type UploadProfile struct {
	SchoolID string `json:"school_id"`
	Access   string `json:"access"`
	Purpose  string `json:"purpose"`
}

type Entitlement struct {
	Purchased bool
	Member    bool
}
type Entitlements interface {
	Lookup(context.Context, Principal, string) (Entitlement, error)
}
type Dependencies struct {
	Authority    Authority
	Catalog      Catalog
	Files        Files
	Moderation   Moderation
	Entitlements Entitlements
}
type Config struct {
	UploadProfiles                                                                          []UploadProfile
	Clock                                                                                   func() time.Time
	Terms                                                                                   Terms
	AllowedTypes                                                                            []string
	MaxFileSize, MaxPackageBytes                                                            int64
	MaxPackageFiles, MaxPackageParts, MaxDirectoryDepth, MaxRecords, MaxOperations, MaxJobs int
	OperationTimeout, PackageTTL, Lease                                                     time.Duration
	MaxAttempts                                                                             int
}
type Replay struct {
	Key, Digest string
	Result      json.RawMessage
}
type Event struct {
	ID, Type, ResourceID, SchoolID, Actor string
	Revision                              int64
	Time                                  time.Time
}
type Job struct {
	ID, Kind, ResourceID, VersionID, RequestID, ReferenceID, State, Owner, ErrorCode string
	Generation                                                                       int64
	Attempts                                                                         int
	LeaseUntil, AvailableAt                                                          time.Time
}

type Repository interface {
	Transaction(context.Context, func(Transaction) error) error
	Claim(context.Context, string, time.Time, time.Duration) (Job, error)
	PendingEvents(context.Context, int) ([]Event, error)
	AcknowledgeEvent(context.Context, string) error
}
type Transaction interface {
	Lock(string) error
	Category(string, int64) (Category, error)
	SaveCategory(Category) error
	Categories(int, string, bool) ([]Category, error)
	Record(string) (Record, error)
	SaveRecord(Record) error
	Version(string) (VersionRecord, error)
	SaveVersion(VersionRecord) error
	List(Filter) ([]Record, error)
	Counts() (records, operations, jobs int, err error)
	Replay(string) (Replay, error)
	SaveReplay(Replay) error
	Grant(string, string) (GrantRecord, error)
	GrantByID(string) (GrantRecord, error)
	Grants(string, int, string) ([]GrantRecord, error)
	SaveGrant(GrantRecord) error
	Directory(string) (Directory, error)
	SaveDirectory(Directory) error
	Directories(string, string, string, int, string) ([]Directory, error)
	Package(string) (PackageRecord, error)
	SavePackage(PackageRecord) error
	Request(string) (RequestRecord, error)
	SaveRequest(RequestRecord) error
	Job(string) (Job, error)
	SaveJob(Job) error
	Event(Event) error
}
