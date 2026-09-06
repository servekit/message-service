// Package region serves the international dial-code directory — static
// reference data (ISO 3166-1 alpha-2 + ITU E.164 + CLDR names) that backs
// region pickers on the business side. No persistence, no config: the table
// in data.go is compiled in.
package region

import (
	"context"

	pb "github.com/servekit/api/gen/go/messaging/v1"
)

// Entry is one row of the compiled dial-code directory (see data.go).
type Entry struct {
	Code     string
	DialCode string
	NameZh   string
	NameEn   string
}

// Service exposes the region directory.
type Service struct{}

// New constructs the region service.
func New() *Service {
	return &Service{}
}

// ListRegionCodes returns the full directory, zh-Hans sorted (data.go order).
func (s *Service) ListRegionCodes(_ context.Context, _ *pb.ListRegionCodesRequest) (*pb.ListRegionCodesResponse, error) {
	codes := make([]*pb.RegionCode, 0, len(regionTable))
	for _, e := range regionTable {
		codes = append(codes, &pb.RegionCode{
			Code:     e.Code,
			DialCode: e.DialCode,
			NameZh:   e.NameZh,
			NameEn:   e.NameEn,
		})
	}
	return &pb.ListRegionCodesResponse{RegionCodes: codes}, nil
}
