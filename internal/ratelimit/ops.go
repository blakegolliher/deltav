package ratelimit

import (
	"sort"
	"strings"
)

// S3 operations, and how a request is recognized as one.
//
// The table below is adapted from tollbooth's internal/s3id/classify.go
// (github.com/blakegolliher/tollbooth, Apache-2.0, Copyright 2026 Blake
// Golliher), so that a rule written for one means the same in the other. It
// is restated here as data, from which the classifier that runs inside Envoy
// is generated (lua.go).
//
// A request has a method and a level: it names no bucket (service), a bucket
// only, or a bucket and a key. Within a method and level the rows are tried
// in order and the first whose conditions hold names the operation. The
// conditions are query keys that must be present (compared as the raw bytes
// on the wire, case-sensitively, as S3 does), at most one query key that
// must have an exact value, and whether the x-amz-copy-source header is
// present. partNumber, versionId and Range never decide anything.

// Levels of a request.
const (
	levelService = 0
	levelBucket  = 1
	levelObject  = 2
)

// opRow is one row of the classification table.
type opRow struct {
	method string
	level  int
	query  []string // keys that must be present
	key    string   // a key that must have the value below ("" if none)
	value  string
	copy   bool   // x-amz-copy-source must be present and not empty
	op     string // "" means the request is not a known operation
}

// Operations that are named without a table row.
const (
	OpPreflight = "Preflight" // any OPTIONS request
	OpUnknown   = "Unknown"   // anything the table does not recognize
)

// A bucket subresource with its GET, PUT and DELETE operations ("" if S3 has
// none).
var bucketSubresources = [][4]string{
	{"location", "GetBucketLocation", "", ""},
	{"tagging", "GetBucketTagging", "PutBucketTagging", "DeleteBucketTagging"},
	{"ownershipControls", "GetBucketOwnershipControls", "PutBucketOwnershipControls", "DeleteBucketOwnershipControls"},
	{"versioning", "GetBucketVersioning", "PutBucketVersioning", ""},
	{"policyStatus", "GetBucketPolicyStatus", "", ""},
	{"policy", "GetBucketPolicy", "PutBucketPolicy", "DeleteBucketPolicy"},
	{"cors", "GetBucketCors", "PutBucketCors", "DeleteBucketCors"},
	{"object-lock", "GetObjectLockConfiguration", "PutObjectLockConfiguration", ""},
	{"acl", "GetBucketAcl", "PutBucketAcl", ""},
	{"uploads", "ListMultipartUploads", "", ""},
	{"versions", "ListObjectVersions", "", ""},
	{"encryption", "GetBucketEncryption", "PutBucketEncryption", "DeleteBucketEncryption"},
	{"lifecycle", "GetBucketLifecycleConfiguration", "PutBucketLifecycleConfiguration", "DeleteBucketLifecycle"},
	{"logging", "GetBucketLogging", "PutBucketLogging", ""},
	{"requestPayment", "GetBucketRequestPayment", "PutBucketRequestPayment", ""},
	{"replication", "GetBucketReplication", "PutBucketReplication", "DeleteBucketReplication"},
	{"publicAccessBlock", "GetPublicAccessBlock", "PutPublicAccessBlock", "DeletePublicAccessBlock"},
	{"notification", "GetBucketNotificationConfiguration", "PutBucketNotificationConfiguration", ""},
	{"accelerate", "GetBucketAccelerateConfiguration", "PutBucketAccelerateConfiguration", ""},
	{"website", "GetBucketWebsite", "PutBucketWebsite", "DeleteBucketWebsite"},
	{"session", "CreateSession", "", ""},
	{"abac", "GetBucketAbac", "PutBucketAbac", ""},
	{"metadataTable", "GetBucketMetadataTableConfiguration", "", "DeleteBucketMetadataTableConfiguration"},
	{"metadataConfiguration", "GetBucketMetadataConfiguration", "", "DeleteBucketMetadataConfiguration"},
	{"metadataInventoryTable", "", "UpdateBucketMetadataInventoryTableConfiguration", ""},
	{"metadataJournalTable", "", "UpdateBucketMetadataJournalTableConfiguration", ""},
	{"metadataAnnotationTable", "", "UpdateBucketMetadataAnnotationTableConfiguration", ""},
}

// A bucket subresource that holds several named configurations: GET with an
// id reads one, GET without lists them.
var bucketConfigured = [][5]string{
	{"analytics", "GetBucketAnalyticsConfiguration", "ListBucketAnalyticsConfigurations", "PutBucketAnalyticsConfiguration", "DeleteBucketAnalyticsConfiguration"},
	{"intelligent-tiering", "GetBucketIntelligentTieringConfiguration", "ListBucketIntelligentTieringConfigurations", "PutBucketIntelligentTieringConfiguration", "DeleteBucketIntelligentTieringConfiguration"},
	{"inventory", "GetBucketInventoryConfiguration", "ListBucketInventoryConfigurations", "PutBucketInventoryConfiguration", "DeleteBucketInventoryConfiguration"},
	{"metrics", "GetBucketMetricsConfiguration", "ListBucketMetricsConfigurations", "PutBucketMetricsConfiguration", "DeleteBucketMetricsConfiguration"},
}

// opTable is the classification table, in the order it is tried.
var opTable = buildOpTable()

func buildOpTable() []opRow {
	var t []opRow
	add := func(method string, level int, op string, query ...string) {
		t = append(t, opRow{method: method, level: level, query: query, op: op})
	}
	add("GET", levelService, "ListBuckets")
	for _, c := range bucketConfigured {
		add("GET", levelBucket, c[1], c[0], "id")
		add("GET", levelBucket, c[2], c[0])
		add("PUT", levelBucket, c[3], c[0])
		add("DELETE", levelBucket, c[4], c[0])
	}
	for _, s := range bucketSubresources {
		for i, method := range []string{"GET", "PUT", "DELETE"} {
			if s[i+1] != "" {
				add(method, levelBucket, s[i+1], s[0])
			}
		}
	}
	t = append(t, opRow{method: "GET", level: levelBucket, key: "list-type", value: "2", op: "ListObjectsV2"})
	add("GET", levelBucket, "ListObjects")
	add("HEAD", levelBucket, "HeadBucket")
	add("PUT", levelBucket, "CreateBucket")
	add("DELETE", levelBucket, "DeleteBucket")
	add("POST", levelBucket, "CreateBucketMetadataTableConfiguration", "metadataTable")
	add("POST", levelBucket, "CreateBucketMetadataConfiguration", "metadataConfiguration")
	add("POST", levelBucket, "DeleteObjects", "delete")
	add("POST", levelBucket, "PostObject")

	add("GET", levelObject, "GetObjectAcl", "acl")
	add("GET", levelObject, "GetObjectAttributes", "attributes")
	add("GET", levelObject, "GetObjectLegalHold", "legal-hold")
	add("GET", levelObject, "GetObjectRetention", "retention")
	add("GET", levelObject, "GetObjectTagging", "tagging")
	add("GET", levelObject, "ListParts", "uploadId")
	add("GET", levelObject, "GetObjectTorrent", "torrent")
	add("GET", levelObject, "GetObjectAnnotation", "annotation", "annotationName")
	add("GET", levelObject, "ListObjectAnnotations", "annotation")
	add("GET", levelObject, "", "uploads")  // a bucket's subresource on a key: nothing S3 defines
	add("GET", levelObject, "", "versions") // the same
	add("GET", levelObject, "GetObject")
	add("HEAD", levelObject, "HeadObject")
	add("DELETE", levelObject, "AbortMultipartUpload", "uploadId")
	add("DELETE", levelObject, "DeleteObjectTagging", "tagging")
	add("DELETE", levelObject, "DeleteObjectAnnotation", "annotation")
	add("DELETE", levelObject, "DeleteObject")
	add("POST", levelObject, "CreateMultipartUpload", "uploads")
	add("POST", levelObject, "CompleteMultipartUpload", "uploadId")
	add("POST", levelObject, "RestoreObject", "restore")
	t = append(t, opRow{method: "POST", level: levelObject, query: []string{"select"}, key: "select-type", value: "2", op: "SelectObjectContent"})
	add("PUT", levelObject, "PutObjectTagging", "tagging")
	add("PUT", levelObject, "PutObjectRetention", "retention")
	add("PUT", levelObject, "PutObjectLegalHold", "legal-hold")
	add("PUT", levelObject, "PutObjectAcl", "acl")
	add("PUT", levelObject, "RenameObject", "renameObject")
	add("PUT", levelObject, "PutObjectAnnotation", "annotation")
	add("PUT", levelObject, "UpdateObjectEncryption", "encryption")
	t = append(t, opRow{method: "PUT", level: levelObject, query: []string{"uploadId", "partNumber"}, copy: true, op: "UploadPartCopy"})
	add("PUT", levelObject, "UploadPart", "uploadId", "partNumber")
	t = append(t, opRow{method: "PUT", level: levelObject, copy: true, op: "CopyObject"})
	add("PUT", levelObject, "PutObject")
	return t
}

// listOps are the operations whose "prefix" query parameter a rule's prefix
// is matched against; for every operation on a key it is matched against the
// key, and for any other operation a rule with a prefix does not match.
var listOps = map[string]bool{
	"ListObjects": true, "ListObjectsV2": true, "ListObjectVersions": true, "ListMultipartUploads": true,
}

// operations maps each operation's name in lower case to S3's spelling.
var operations = func() map[string]string {
	m := map[string]string{}
	for _, r := range opTable {
		if r.op != "" {
			m[strings.ToLower(r.op)] = r.op
		}
	}
	return m
}()

// Operations returns every operation a rule can name, sorted.
func Operations() []string {
	out := make([]string, 0, len(operations))
	for _, op := range operations {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}

// expandOperation turns what a rule says into operation names: one name, in
// any letter case, or a family, which is the start of a name followed by "*"
// (List*). It returns nil if nothing is named.
func expandOperation(s string) []string {
	if family, ok := strings.CutSuffix(s, "*"); ok {
		if family == "" {
			return nil // a bare "*": leave the filter out to mean every operation
		}
		var out []string
		for lower, op := range operations {
			if strings.HasPrefix(lower, strings.ToLower(family)) {
				out = append(out, op)
			}
		}
		sort.Strings(out)
		return out
	}
	if op, ok := operations[strings.ToLower(s)]; ok {
		return []string{op}
	}
	return nil
}
