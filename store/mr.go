package store

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// MergeRequest models a change-tracking request between two revisions.
type MergeRequest struct {
	ID          int64
	RepoID      int64
	IID         int64
	Title       string
	Description string
	Source      string // revision id or branch name
	Target      string // revision id or branch name
	State       string // "open" | "merged" | "closed"
	AuthorID    *int64
	Created     time.Time
	Updated     time.Time
}

// Review is a reviewer's verdict on a merge request.
type Review struct {
	ID         int64
	MRID       int64
	ReviewerID *int64
	State      string // "approved" | "request_changes" | "comment"
	Body       string
	Created    time.Time
}

// Comment is a discussion line on a merge request.
type Comment struct {
	ID       int64
	MRID     int64
	AuthorID *int64
	Body     string
	Path     string
	Created  time.Time
}

// CreateMergeRequest inserts a new MR and returns it. IID is assigned
// monotonically per repo.
func (s *CentralStore) CreateMergeRequest(repoID int64, mr *MergeRequest) (*MergeRequest, error) {
	now := time.Now().UTC().UnixMilli()
	var maxIID *int64
	if err := s.d.gdb.Model(&mergeRequestRow{}).Where("repo_id=?", repoID).Select("COALESCE(MAX(iid),0)+1").Scan(&maxIID).Error; err != nil {
		return nil, fmt.Errorf("assign MR iid: %w", err)
	}
	iid := int64(1)
	if maxIID != nil {
		iid = *maxIID
	}
	row := &mergeRequestRow{
		RepoID: repoID, IID: iid, Title: mr.Title, Description: mr.Description,
		Source: mr.Source, Target: mr.Target, State: mr.State,
		AuthorID: mr.AuthorID, Created: now, Updated: now,
	}
	if err := s.d.gdb.Create(row).Error; err != nil {
		return nil, err
	}
	return &MergeRequest{
		ID: row.ID, RepoID: repoID, IID: iid, Title: mr.Title, Description: mr.Description,
		Source: mr.Source, Target: mr.Target, State: mr.State, AuthorID: mr.AuthorID,
		Created: time.UnixMilli(now), Updated: time.UnixMilli(now),
	}, nil
}

// GetMergeRequest returns an MR by repo + iid.
func (s *CentralStore) GetMergeRequest(repoID, iid int64) (*MergeRequest, error) {
	var row mergeRequestRow
	err := s.d.gdb.Where("repo_id=? AND iid=?", repoID, iid).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &MergeRequest{
		ID: row.ID, RepoID: row.RepoID, IID: row.IID, Title: row.Title, Description: row.Description,
		Source: row.Source, Target: row.Target, State: row.State, AuthorID: row.AuthorID,
		Created: time.UnixMilli(row.Created), Updated: time.UnixMilli(row.Updated),
	}, nil
}

// ListMergeRequests returns MRs for a repo, optionally filtered by state.
func (s *CentralStore) ListMergeRequests(repoID int64, state string) ([]*MergeRequest, error) {
	q := s.d.gdb.Where("repo_id=?", repoID)
	if state != "" {
		q = q.Where("state=?", state)
	}
	var rows []mergeRequestRow
	if err := q.Order("iid DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*MergeRequest, 0, len(rows))
	for i := range rows {
		out = append(out, &MergeRequest{
			ID: rows[i].ID, RepoID: rows[i].RepoID, IID: rows[i].IID, Title: rows[i].Title,
			Description: rows[i].Description, Source: rows[i].Source, Target: rows[i].Target,
			State: rows[i].State, AuthorID: rows[i].AuthorID,
			Created: time.UnixMilli(rows[i].Created), Updated: time.UnixMilli(rows[i].Updated),
		})
	}
	return out, nil
}

// UpdateMergeRequestState updates an MR state (merged/closed/reopen).
func (s *CentralStore) UpdateMergeRequestState(repoID, iid int64, state string) error {
	return s.d.gdb.Model(&mergeRequestRow{}).Where("repo_id=? AND iid=?", repoID, iid).Updates(map[string]any{
		"state": state, "updated": time.Now().UTC().UnixMilli(),
	}).Error
}

// AddReview appends a review to an MR.
func (s *CentralStore) AddReview(mrID int64, reviewerID *int64, state, body string) (*Review, error) {
	now := time.Now().UTC().UnixMilli()
	row := &mrReviewRow{MRID: mrID, ReviewerID: reviewerID, State: state, Body: body, Created: now}
	if err := s.d.gdb.Create(row).Error; err != nil {
		return nil, err
	}
	return &Review{ID: row.ID, MRID: mrID, ReviewerID: reviewerID, State: state, Body: body, Created: time.UnixMilli(now)}, nil
}

// ListReviews returns reviews for an MR.
func (s *CentralStore) ListReviews(mrID int64) ([]*Review, error) {
	var rows []mrReviewRow
	if err := s.d.gdb.Where("mr_id=?", mrID).Order("created").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Review, 0, len(rows))
	for i := range rows {
		out = append(out, &Review{ID: rows[i].ID, MRID: rows[i].MRID, ReviewerID: rows[i].ReviewerID, State: rows[i].State, Body: rows[i].Body, Created: time.UnixMilli(rows[i].Created)})
	}
	return out, nil
}

// AddComment appends a comment to an MR.
func (s *CentralStore) AddComment(mrID int64, authorID *int64, body, path string) (*Comment, error) {
	now := time.Now().UTC().UnixMilli()
	row := &mrCommentRow{MRID: mrID, AuthorID: authorID, Body: body, Path: strPtr(path), Created: now}
	if err := s.d.gdb.Create(row).Error; err != nil {
		return nil, err
	}
	return &Comment{ID: row.ID, MRID: mrID, AuthorID: authorID, Body: body, Path: path, Created: time.UnixMilli(now)}, nil
}

// ListComments returns comments for an MR.
func (s *CentralStore) ListComments(mrID int64) ([]*Comment, error) {
	var rows []mrCommentRow
	if err := s.d.gdb.Where("mr_id=?", mrID).Order("created").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*Comment, 0, len(rows))
	for i := range rows {
		path := ""
		if rows[i].Path != nil {
			path = *rows[i].Path
		}
		out = append(out, &Comment{ID: rows[i].ID, MRID: rows[i].MRID, AuthorID: rows[i].AuthorID, Body: rows[i].Body, Path: path, Created: time.UnixMilli(rows[i].Created)})
	}
	return out, nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
