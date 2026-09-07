// Query side of the SMS domain: record Get/List (offset + cursor)/Stats/
// Regions. The send path lives in send.go.
package sms

import (
	"context"
	"fmt"
	"time"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/utils/pagination"
	"github.com/servekit/message-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"
)

// GetSMS returns a single SMS record by ID.
func (s *Service) GetSMS(ctx context.Context, req *pb.GetSMSRequest) (*pb.SMSRecord, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
	}
	record, err := dal.GetSMSRecord(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	return toProtoSMSRecord(record), nil
}

// ListSMS returns a paginated list of SMS records matching the filter.
func (s *Service) ListSMS(ctx context.Context, req *pb.ListSMSRequest) (*pb.ListSMSResponse, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
	}
	f := dal.SmsListFilter{
		Vendor:        req.GetVendor(),
		Scene:         req.GetScene(),
		Status:        req.GetStatus(),
		RegionCode:    req.GetRegionCode(),
		Phone:         req.GetPhone(),
		AppKey:        req.GetAppKey(),
		SortField:     req.GetSortField(),
		SortDirection: req.GetSortDirection(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	result, err := dal.ListSMSRecords(ctx, s.db, f, dbx.PageParams{
		Page:     int(req.GetPage()),
		PageSize: int(req.GetPageSize()),
		Count:    true,
	})
	if err != nil {
		return nil, err
	}

	protoRecords := make([]*pb.SMSRecord, len(result.List))
	for i, r := range result.List {
		protoRecords[i] = toProtoSMSRecord(r)
	}

	return &pb.ListSMSResponse{
		Records:    protoRecords,
		Total:      int32(result.Total),
		TotalPages: int32(result.TotalPages),
		HasMore:    req.GetPage() < int32(result.TotalPages),
	}, nil
}

// ListSMSByCursor is the cursor-paginated counterpart of ListSMS.
// Prefer this over ListSMS for large datasets or when COUNT(*) is expensive —
// set include_total = true to opt in to a count query.
func (s *Service) ListSMSByCursor(ctx context.Context, req *pb.ListSMSByCursorRequest) (*pb.ListSMSByCursorResponse, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
	}
	f := dal.SmsListFilter{
		Vendor:        req.GetVendor(),
		Scene:         req.GetScene(),
		Status:        req.GetStatus(),
		RegionCode:    req.GetRegionCode(),
		Phone:         req.GetPhone(),
		AppKey:        req.GetAppKey(),
		SortField:     req.GetSortField(),
		SortDirection: req.GetSortDirection(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	pg := dbx.Pagination{PageSize: int(req.GetPageSize())}
	var afterCreatedAt time.Time
	if token := req.GetPageToken(); token != "" {
		cursor, err := pagination.DecodePageCursor(token)
		if err != nil {
			return nil, xcodes.ErrBadRequest.Wrap(err)
		}
		pg.AfterID = cursor.ID
		afterCreatedAt = pagination.CursorToCreatedAt(cursor.CreatedAt)
	}

	records, err := dal.ListSMSByCursor(ctx, s.db, f, pg, afterCreatedAt)
	if err != nil {
		return nil, err
	}

	trimmed, hasNext := dbx.TrimPage(records, pg.PageSize)

	protoRecords := make([]*pb.SMSRecord, len(trimmed))
	for i, r := range trimmed {
		protoRecords[i] = toProtoSMSRecord(r)
	}

	var total int32
	if req.GetIncludeTotal() {
		// Cheap path: if this is the first page and it fit in one go,
		// total == len(trimmed). Otherwise run a real count.
		if !hasNext && pg.AfterID == 0 {
			total = int32(len(trimmed))
		} else {
			count, err := dal.CountSMSRecords(ctx, s.db, f)
			if err != nil {
				return nil, err
			}
			total = int32(count)
		}
	}

	var nextToken string
	if hasNext {
		last := trimmed[len(trimmed)-1]
		nextToken = pagination.EncodePageCursor(pagination.PageCursor{
			ID:        last.ID,
			CreatedAt: pagination.CursorFromCreatedAt(last.CreatedAt),
		})
	}

	return &pb.ListSMSByCursorResponse{
		Records:       protoRecords,
		Total:         total,
		NextPageToken: nextToken,
	}, nil
}

// GetSMSStats returns aggregated statistics for SMS messages matching the filter.
func (s *Service) GetSMSStats(ctx context.Context, req *pb.GetSMSStatsRequest) (*pb.SMSStatsResponse, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
	}
	f := dal.SmsStatsFilter{
		Vendor: req.GetVendor(),
		Scene:  req.GetScene(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	stats, err := dal.CountSMSStats(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	vendorStats, err := dal.ListSMSVendorStats(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	vendors := make([]*pb.SmsVendorStats, len(vendorStats))
	for i, vs := range vendorStats {
		vendors[i] = &pb.SmsVendorStats{
			Vendor: vs.Vendor,
			Total:  vs.Total,
			Sent:   vs.Sent,
			Failed: vs.Failed,
		}
	}

	return &pb.SMSStatsResponse{
		Total:       stats.Total,
		Sent:        stats.Sent,
		Failed:      stats.Failed,
		SuccessRate: stats.SuccessRate,
		Vendors:     vendors,
	}, nil
}

// ListSMSRegions returns all distinct region_code values, for frontend SMS
// list filter dropdowns.
func (s *Service) ListSMSRegions(ctx context.Context, _ *pb.ListSMSRegionsRequest) (*pb.ListSMSRegionsResponse, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
	}
	regions, err := dal.ListSMSRegions(ctx, s.db)
	if err != nil {
		return nil, err
	}
	return &pb.ListSMSRegionsResponse{RegionCodes: regions}, nil
}
