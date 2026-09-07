// Package admin implements messaging.v1.MessageAdminService: CRUD over the
// platform resource model (apps / channel accounts / signatures / templates
// / policies). Every mutation writes the DB, then refreshes the in-process
// registry snapshot immediately — cross-node convergence is handled by the
// cron refresh.
package admin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	"github.com/servekit/go-common/jsonx"
	provemail "github.com/servekit/message-service/internal/provider/email"
	provesms "github.com/servekit/message-service/internal/provider/sms"
	"github.com/servekit/message-service/internal/registry"
	"github.com/servekit/message-service/internal/send"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"

	"google.golang.org/protobuf/types/known/emptypb"
	"gorm.io/gorm"
)

// Service is the admin domain service.
type Service struct {
	db  *gorm.DB
	reg *registry.Registry
	gid gidservice.Service
}

// New constructs the admin service.
func New(db *gorm.DB, reg *registry.Registry, gid gidservice.Service) *Service {
	return &Service{db: db, reg: reg, gid: gid}
}

// refresh reloads the snapshot after a committed mutation. Failure is
// logged, not returned: the write committed, and the cron refresh
// converges (telemetry admin pattern).
func (s *Service) refresh(ctx context.Context) {
	if err := s.reg.Refresh(ctx); err != nil {
		slog.Error("admin: registry refresh after mutation (cron will converge)", "error", err)
	}
}

func (s *Service) nextID(ctx context.Context) (int64, error) {
	return gidservice.NextID(ctx, s.gid)
}

// --- Apps ---

// CreateApp registers a calling app and returns the plaintext app_secret
// exactly once.
func (s *Service) CreateApp(ctx context.Context, req *pb.CreateAppRequest) (*pb.CreateAppResponse, error) {
	if _, err := dal.GetAppByKey(ctx, s.db, req.GetAppKey()); err == nil {
		return nil, xcodes.ErrBadRequest.New(fmt.Sprintf("app_key %q already exists", req.GetAppKey()))
	}
	secret, err := mintSecret()
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	id, err := s.nextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	app := &models.MessageApp{
		ID:              id,
		AppKey:          req.GetAppKey(),
		AppSecret:       secret,
		Name:            req.GetName(),
		SMSDailyLimit:   req.GetSmsDailyLimit(),
		EmailDailyLimit: req.GetEmailDailyLimit(),
	}
	if err := dal.CreateApp(ctx, s.db, app); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.CreateAppResponse{App: appToProto(app), AppSecret: secret}, nil
}

// GetApp returns one app by id.
func (s *Service) GetApp(ctx context.Context, req *pb.GetAppRequest) (*pb.GetAppResponse, error) {
	app, err := dal.GetApp(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	return &pb.GetAppResponse{App: appToProto(app)}, nil
}

// UpdateApp tweaks app metadata; absent optional fields keep their values.
func (s *Service) UpdateApp(ctx context.Context, req *pb.UpdateAppRequest) (*pb.UpdateAppResponse, error) {
	app, err := dal.GetApp(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if req.Name != nil {
		app.Name = req.GetName()
	}
	if req.Disabled != nil {
		app.Disabled = req.GetDisabled()
	}
	if req.SmsDailyLimit != nil {
		app.SMSDailyLimit = req.GetSmsDailyLimit()
	}
	if req.EmailDailyLimit != nil {
		app.EmailDailyLimit = req.GetEmailDailyLimit()
	}
	if err := dal.UpdateApp(ctx, s.db, app); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.UpdateAppResponse{App: appToProto(app)}, nil
}

// RotateAppSecret invalidates the current secret and returns a new
// plaintext exactly once.
func (s *Service) RotateAppSecret(ctx context.Context, req *pb.RotateAppSecretRequest) (*pb.RotateAppSecretResponse, error) {
	app, err := dal.GetApp(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	secret, err := mintSecret()
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	app.AppSecret = secret
	if err := dal.UpdateApp(ctx, s.db, app); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.RotateAppSecretResponse{App: appToProto(app), AppSecret: secret}, nil
}

// ListApps returns all apps.
func (s *Service) ListApps(ctx context.Context, _ *pb.ListAppsRequest) (*pb.ListAppsResponse, error) {
	apps := s.reg.Current().Apps()
	out := make([]*pb.MessageAppInfo, len(apps))
	for i, a := range apps {
		out[i] = appToProto(a)
	}
	return &pb.ListAppsResponse{Apps: out}, nil
}

// DeleteApp soft-deletes an app; its sends fail immediately.
func (s *Service) DeleteApp(ctx context.Context, req *pb.DeleteAppRequest) (*emptypb.Empty, error) {
	if _, err := dal.GetApp(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	if err := dal.DeleteApp(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &emptypb.Empty{}, nil
}

// --- Channel accounts ---

// CreateChannelAccount adds a vendor account to the platform pool.
func (s *Service) CreateChannelAccount(ctx context.Context, req *pb.CreateChannelAccountRequest) (*pb.CreateChannelAccountResponse, error) {
	ac, vendor, channel, err := credentialsToModel(req.GetName(), req.GetCredentials())
	if err != nil {
		return nil, err
	}
	id, err := s.nextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	account := &models.MessageChannelAccount{
		ID:      id,
		Channel: int32(channel),
		Vendor:  vendor,
		Name:    req.GetName(),
		Remark:  req.GetRemark(),
		Config:  ac,
	}
	if err := dal.CreateChannelAccount(ctx, s.db, account); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.CreateChannelAccountResponse{Account: s.accountToProto(ctx, account)}, nil
}

// UpdateChannelAccount edits remark/disabled or replaces credentials.
func (s *Service) UpdateChannelAccount(ctx context.Context, req *pb.UpdateChannelAccountRequest) (*pb.UpdateChannelAccountResponse, error) {
	account, err := dal.GetChannelAccount(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if req.Remark != nil {
		account.Remark = req.GetRemark()
	}
	if req.Disabled != nil {
		account.Disabled = req.GetDisabled()
	}
	if req.GetCredentials() != nil {
		ac, vendor, channel, err := credentialsToModel(account.Name, req.GetCredentials())
		if err != nil {
			return nil, err
		}
		account.Config = ac
		account.Vendor = vendor
		account.Channel = int32(channel)
	}
	if err := dal.UpdateChannelAccount(ctx, s.db, account); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.UpdateChannelAccountResponse{Account: s.accountToProto(ctx, account)}, nil
}

// DeleteChannelAccount soft-deletes an account from the pool.
func (s *Service) DeleteChannelAccount(ctx context.Context, req *pb.DeleteChannelAccountRequest) (*emptypb.Empty, error) {
	if _, err := dal.GetChannelAccount(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	if err := dal.DeleteChannelAccount(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &emptypb.Empty{}, nil
}

// ListChannelAccounts returns the full pool with secrets masked.
func (s *Service) ListChannelAccounts(ctx context.Context, _ *pb.ListChannelAccountsRequest) (*pb.ListChannelAccountsResponse, error) {
	accounts := s.reg.Current().ChannelAccounts()
	out := make([]*pb.ChannelAccountInfo, len(accounts))
	for i, a := range accounts {
		out[i] = s.accountToProto(ctx, a)
	}
	return &pb.ListChannelAccountsResponse{Accounts: out}, nil
}

// accountToProto renders a DB account row. Credentials are decoded and
// echoed with SECRET FIELDS EMPTIED (write-only); a corrupt config JSON
// yields an account with an empty credentials oneof rather than an error
// (list must stay usable for triage).
func (s *Service) accountToProto(_ context.Context, a *models.MessageChannelAccount) *pb.ChannelAccountInfo {
	info := &pb.ChannelAccountInfo{
		Id:       a.ID,
		Name:     a.Name,
		Disabled: a.Disabled,
		Remark:   a.Remark,
		// created_at/updated_at filled below
	}
	if !a.CreatedAt.IsZero() {
		info.CreatedAt = a.CreatedAt.Unix()
	}
	if !a.UpdatedAt.IsZero() {
		info.UpdatedAt = a.UpdatedAt.Unix()
	}
	switch pb.TemplateChannel(a.Channel) {
	case pb.TemplateChannel_TEMPLATE_CHANNEL_SMS:
		info.Vendor = &pb.ChannelAccountInfo_SmsVendor{SmsVendor: pb.SmsVendor(a.Vendor)}
		var ac provesms.AccountConfig
		if err := jsonx.Unmarshal(a.Config, &ac); err == nil {
			info.Credentials = smsAccountToProtoCredentials(&ac)
		}
	case pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL:
		info.Vendor = &pb.ChannelAccountInfo_EmailVendor{EmailVendor: pb.EmailVendor(a.Vendor)}
		var ac provemail.AccountConfig
		if err := jsonx.Unmarshal(a.Config, &ac); err == nil {
			info.Credentials = emailAccountToProtoCredentials(&ac)
		}
	}
	return info
}

// --- Signatures ---

// CreateSignature registers a signature with its account bindings.
func (s *Service) CreateSignature(ctx context.Context, req *pb.CreateSignatureRequest) (*pb.CreateSignatureResponse, error) {
	if err := s.validateBindingAccounts(ctx, req.GetAccountIds()); err != nil {
		return nil, err
	}
	id, err := s.nextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	sig := &models.MessageSignature{ID: id, Name: req.GetName(), Remark: req.GetRemark()}
	if err := dal.CreateSignature(ctx, s.db, sig, req.GetAccountIds()); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.CreateSignatureResponse{Signature: s.signatureToProto(sig, req.GetAccountIds())}, nil
}

// UpdateSignature replaces remark/disabled/bindings.
func (s *Service) UpdateSignature(ctx context.Context, req *pb.UpdateSignatureRequest) (*pb.UpdateSignatureResponse, error) {
	sig, err := dal.GetSignature(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if req.Remark != nil {
		sig.Remark = req.GetRemark()
	}
	if req.Disabled != nil {
		sig.Disabled = req.GetDisabled()
	}
	accounts := s.reg.Current().SignatureAccounts(sig.ID)
	if req.GetAccountIds() != nil {
		accounts = req.GetAccountIds().GetIds()
		if err := s.validateBindingAccounts(ctx, accounts); err != nil {
			return nil, err
		}
	}
	if err := dal.UpdateSignature(ctx, s.db, sig, accounts); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.UpdateSignatureResponse{Signature: s.signatureToProto(sig, accounts)}, nil
}

// DeleteSignature soft-deletes a signature and its bindings.
func (s *Service) DeleteSignature(ctx context.Context, req *pb.DeleteSignatureRequest) (*emptypb.Empty, error) {
	if _, err := dal.GetSignature(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	if err := dal.DeleteSignature(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &emptypb.Empty{}, nil
}

// ListSignatures returns all signatures with bindings.
func (s *Service) ListSignatures(ctx context.Context, _ *pb.ListSignaturesRequest) (*pb.ListSignaturesResponse, error) {
	sigs := s.reg.Current().Signatures()
	out := make([]*pb.SignatureInfo, len(sigs))
	for i, sig := range sigs {
		out[i] = s.signatureToProto(sig, s.reg.Current().SignatureAccounts(sig.ID))
	}
	return &pb.ListSignaturesResponse{Signatures: out}, nil
}

// validateBindingAccounts checks every bound account exists and is an SMS
// channel account (signatures are SMS-only concepts).
func (s *Service) validateBindingAccounts(ctx context.Context, accountIDs []int64) error {
	snap := s.reg.Current()
	for _, id := range accountIDs {
		account := snap.ChannelAccount(id)
		if account == nil {
			return xcodes.ErrChannelAccountNotFound.New(fmt.Sprintf("account %d not found (or deleted)", id))
		}
		if pb.TemplateChannel(account.Channel) != pb.TemplateChannel_TEMPLATE_CHANNEL_SMS {
			return xcodes.ErrBadRequest.New(fmt.Sprintf("account %d (%s) is not an SMS account", id, account.Name))
		}
	}
	return nil
}

// --- Templates ---

// CreateTemplate registers a template definition.
func (s *Service) CreateTemplate(ctx context.Context, req *pb.CreateTemplateRequest) (*pb.CreateTemplateResponse, error) {
	t, err := templateToModel(req.GetTemplate(), req.GetAppId())
	if err != nil {
		return nil, err
	}
	if req.GetTemplate().GetName() != "" {
		t.Name = req.GetTemplate().GetName()
	}
	id, err := s.nextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	t.ID = id
	if err := dal.CreateTemplate(ctx, s.db, t); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.CreateTemplateResponse{Template: templateToProto(t)}, nil
}

// UpdateTemplate fully replaces a template definition.
func (s *Service) UpdateTemplate(ctx context.Context, req *pb.UpdateTemplateRequest) (*pb.UpdateTemplateResponse, error) {
	existing, err := dal.GetTemplate(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	t, err := templateToModel(req.GetTemplate(), existing.AppID)
	if err != nil {
		return nil, err
	}
	t.ID = existing.ID
	t.AppID = existing.AppID
	if err := dal.UpdateTemplate(ctx, s.db, t); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.UpdateTemplateResponse{Template: templateToProto(t)}, nil
}

// DeleteTemplate soft-deletes a template.
func (s *Service) DeleteTemplate(ctx context.Context, req *pb.DeleteTemplateRequest) (*emptypb.Empty, error) {
	if _, err := dal.GetTemplate(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	if err := dal.DeleteTemplate(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &emptypb.Empty{}, nil
}

// ListTemplates filters by app (0 = all) and channel (0 = all).
func (s *Service) ListTemplates(ctx context.Context, req *pb.ListTemplatesRequest) (*pb.ListTemplatesResponse, error) {
	templates := s.reg.Current().Templates(req.GetAppId(), int32(req.GetChannel()))
	out := make([]*pb.TemplateInfo, len(templates))
	for i, t := range templates {
		out[i] = templateToProto(t)
	}
	return &pb.ListTemplatesResponse{Templates: out}, nil
}

// --- Policies ---

// CreatePolicy binds (app, channel, scene) to a template + route chains.
func (s *Service) CreatePolicy(ctx context.Context, req *pb.CreatePolicyRequest) (*pb.CreatePolicyResponse, error) {
	channel, scene, err := sceneFromProto(req.GetScene())
	if err != nil {
		return nil, err
	}
	app := s.reg.Current().AppByID(req.GetAppId())
	if app == nil {
		return nil, xcodes.ErrAppNotFound.New(fmt.Sprintf("app %d not found", req.GetAppId()))
	}
	if s.reg.Current().Policy(req.GetAppId(), int32(channel), scene) != nil {
		return nil, xcodes.ErrBadRequest.New(fmt.Sprintf("policy for app %q scene already exists", app.AppKey))
	}
	if err := s.validatePolicy(ctx, channel, req.GetTemplateId(), req.GetRoutes(), req.GetIntlRoutes()); err != nil {
		return nil, err
	}
	id, err := s.nextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	routes, err := send.MarshalRoutes(routesFromProto(req.GetRoutes()))
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}
	intlRoutes, err := send.MarshalRoutes(routesFromProto(req.GetIntlRoutes()))
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}
	policy := &models.MessagePolicy{
		ID:         id,
		AppID:      req.GetAppId(),
		Channel:    int32(channel),
		Scene:      scene,
		TemplateID: req.GetTemplateId(),
		Routes:     routes,
		IntlRoutes: intlRoutes,
	}
	if err := dal.CreatePolicy(ctx, s.db, policy); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.CreatePolicyResponse{Policy: policyToProto(policy, req.GetRoutes(), req.GetIntlRoutes())}, nil
}

// UpdatePolicy fully replaces template + route chains.
func (s *Service) UpdatePolicy(ctx context.Context, req *pb.UpdatePolicyRequest) (*pb.UpdatePolicyResponse, error) {
	policy, err := dal.GetPolicy(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if err := s.validatePolicy(ctx, pb.TemplateChannel(policy.Channel), req.GetTemplateId(), req.GetRoutes(), req.GetIntlRoutes()); err != nil {
		return nil, err
	}
	routes, err := send.MarshalRoutes(routesFromProto(req.GetRoutes()))
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}
	intlRoutes, err := send.MarshalRoutes(routesFromProto(req.GetIntlRoutes()))
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}
	policy.TemplateID = req.GetTemplateId()
	if req.Disabled != nil {
		policy.Disabled = req.GetDisabled()
	}
	policy.Routes = routes
	policy.IntlRoutes = intlRoutes
	if err := dal.UpdatePolicy(ctx, s.db, policy); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.UpdatePolicyResponse{Policy: policyToProto(policy, req.GetRoutes(), req.GetIntlRoutes())}, nil
}

// DeletePolicy removes the binding; sends fail closed from then on.
func (s *Service) DeletePolicy(ctx context.Context, req *pb.DeletePolicyRequest) (*emptypb.Empty, error) {
	if _, err := dal.GetPolicy(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	if err := dal.DeletePolicy(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &emptypb.Empty{}, nil
}

// ListPolicies filters by app (0 = all) and channel (0 = all).
func (s *Service) ListPolicies(ctx context.Context, req *pb.ListPoliciesRequest) (*pb.ListPoliciesResponse, error) {
	policies := s.reg.Current().Policies(req.GetAppId(), int32(req.GetChannel()))
	out := make([]*pb.PolicyInfo, len(policies))
	for i, p := range policies {
		routes, _ := send.ParseRoutes(p.Routes)
		intlRoutes, _ := send.ParseRoutes(p.IntlRoutes)
		out[i] = policyToProto(p, routesToProto(routes), routesToProto(intlRoutes))
	}
	return &pb.ListPoliciesResponse{Policies: out}, nil
}

// validatePolicy checks: template exists with matching channel; every
// route account exists, is enabled, and matches the channel; SMS routes
// carry a signature bound to that account; email routes carry none.
func (s *Service) validatePolicy(ctx context.Context, channel pb.TemplateChannel, templateID int64, routes, intlRoutes []*pb.RouteRule) error {
	snap := s.reg.Current()
	t := snap.Template(templateID)
	if t == nil || t.Disabled {
		return xcodes.ErrTemplateNotFound.New(fmt.Sprintf("template %d not found (or disabled)", templateID))
	}
	if pb.TemplateChannel(t.Channel) != channel {
		return xcodes.ErrBadRequest.New(fmt.Sprintf("template %d channel %s does not match policy channel %s",
			templateID, pb.TemplateChannel(t.Channel), channel))
	}
	if channel == pb.TemplateChannel_TEMPLATE_CHANNEL_SMS && len(intlRoutes) == 0 {
		// Allowed: policy rejects intl destinations at send time. Recorded
		// here as a no-op check for readability.
		_ = intlRoutes
	}
	check := func(rs []*pb.RouteRule, requireIntlEligible bool) error {
		for _, r := range rs {
			account := snap.ChannelAccount(r.GetAccountId())
			if account == nil || account.Disabled {
				return xcodes.ErrChannelAccountNotFound.New(fmt.Sprintf("route account %d not found or disabled", r.GetAccountId()))
			}
			if pb.TemplateChannel(account.Channel) != channel {
				return xcodes.ErrBadRequest.New(fmt.Sprintf("route account %d (%s) channel mismatch", r.GetAccountId(), account.Name))
			}
			if channel == pb.TemplateChannel_TEMPLATE_CHANNEL_SMS {
				if r.GetSignatureId() == 0 {
					return xcodes.ErrBadRequest.New("sms routes require a signature")
				}
				sig := snap.Signature(r.GetSignatureId())
				if sig == nil || sig.Disabled {
					return xcodes.ErrSignatureNotFound.New(fmt.Sprintf("route signature %d not found or disabled", r.GetSignatureId()))
				}
				if !snap.SignatureBound(r.GetSignatureId(), r.GetAccountId()) {
					return xcodes.ErrSignatureNotBound.New(fmt.Sprintf(
						"signature %q is not bound to account %q", sig.Name, account.Name))
				}
			} else if r.GetSignatureId() != 0 {
				return xcodes.ErrBadRequest.New("email routes must not set a signature")
			}
		}
		return nil
	}
	if err := check(routes, false); err != nil {
		return err
	}
	return check(intlRoutes, true)
}

// --- conversion helpers ---

func appToProto(a *models.MessageApp) *pb.MessageAppInfo {
	info := &pb.MessageAppInfo{
		Id:              a.ID,
		AppKey:          a.AppKey,
		Name:            a.Name,
		Disabled:        a.Disabled,
		SmsDailyLimit:   a.SMSDailyLimit,
		EmailDailyLimit: a.EmailDailyLimit,
	}
	if !a.CreatedAt.IsZero() {
		info.CreatedAt = a.CreatedAt.Unix()
	}
	if !a.UpdatedAt.IsZero() {
		info.UpdatedAt = a.UpdatedAt.Unix()
	}
	return info
}

func (s *Service) signatureToProto(sig *models.MessageSignature, accountIDs []int64) *pb.SignatureInfo {
	info := &pb.SignatureInfo{
		Id:         sig.ID,
		Name:       sig.Name,
		Disabled:   sig.Disabled,
		Remark:     sig.Remark,
		AccountIds: accountIDs,
	}
	if !sig.CreatedAt.IsZero() {
		info.CreatedAt = sig.CreatedAt.Unix()
	}
	if !sig.UpdatedAt.IsZero() {
		info.UpdatedAt = sig.UpdatedAt.Unix()
	}
	return info
}

// sceneFromProto extracts (channel, scene int32) from the PolicyScene
// oneof.
func sceneFromProto(ps *pb.PolicyScene) (pb.TemplateChannel, int32, error) {
	switch sc := ps.GetScene().(type) {
	case *pb.PolicyScene_EmailScene:
		if sc.EmailScene == pb.EmailScene_EMAIL_SCENE_UNSPECIFIED {
			return 0, 0, xcodes.ErrBadRequest.New("email_scene is required")
		}
		return pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL, int32(sc.EmailScene), nil
	case *pb.PolicyScene_SmsScene:
		if sc.SmsScene == pb.SmsScene_SMS_SCENE_UNSPECIFIED {
			return 0, 0, xcodes.ErrBadRequest.New("sms_scene is required")
		}
		return pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, int32(sc.SmsScene), nil
	default:
		return 0, 0, xcodes.ErrBadRequest.New("scene is required")
	}
}

func routesFromProto(rs []*pb.RouteRule) []send.Route {
	out := make([]send.Route, len(rs))
	for i, r := range rs {
		out[i] = send.Route{AccountID: r.GetAccountId(), SignatureID: r.GetSignatureId(), Weight: r.GetWeight()}
	}
	return out
}

func routesToProto(rs []send.Route) []*pb.RouteRule {
	out := make([]*pb.RouteRule, len(rs))
	for i, r := range rs {
		out[i] = &pb.RouteRule{AccountId: r.AccountID, SignatureId: r.SignatureID, Weight: r.Weight}
	}
	return out
}

func policyToProto(p *models.MessagePolicy, routes, intlRoutes []*pb.RouteRule) *pb.PolicyInfo {
	info := &pb.PolicyInfo{
		Id:         p.ID,
		AppId:      p.AppID,
		Channel:    pb.TemplateChannel(p.Channel),
		TemplateId: p.TemplateID,
		Routes:     routes,
		IntlRoutes: intlRoutes,
		Disabled:   p.Disabled,
	}
	switch pb.TemplateChannel(p.Channel) {
	case pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL:
		info.EmailScene = pb.EmailScene(p.Scene)
	case pb.TemplateChannel_TEMPLATE_CHANNEL_SMS:
		info.SmsScene = pb.SmsScene(p.Scene)
	}
	if !p.CreatedAt.IsZero() {
		info.CreatedAt = p.CreatedAt.Unix()
	}
	if !p.UpdatedAt.IsZero() {
		info.UpdatedAt = p.UpdatedAt.Unix()
	}
	return info
}

func templateToModel(t *pb.TemplateInfo, appID int64) (*models.MessageTemplate, error) {
	channel := t.GetChannel()
	kind := t.GetKind()
	var content models.RawJSON
	var err error
	switch c := t.GetContent().(type) {
	case *pb.TemplateInfo_Email:
		content, err = send.MarshalEmailContent(&send.EmailContent{
			Subject:  c.Email.GetSubject(),
			TextBody: c.Email.GetTextBody(),
			HTMLBody: c.Email.GetHtmlBody(),
		})
	case *pb.TemplateInfo_VendorCodes:
		codes := make([]send.VendorCode, 0, len(c.VendorCodes.GetCodes()))
		for _, vc := range c.VendorCodes.GetCodes() {
			codes = append(codes, send.VendorCode{Vendor: vc.GetVendor(), TemplateCode: vc.GetTemplateCode()})
		}
		content, err = send.MarshalVendorCodes(codes)
	case *pb.TemplateInfo_SmsContent:
		content, err = send.MarshalSmsContent(&send.SmsContent{Content: c.SmsContent.GetContent()})
	default:
		err = xcodes.ErrInvalidTemplateContent.New("content is required")
	}
	if err != nil {
		return nil, xcodes.ErrInvalidTemplateContent.Wrap(err)
	}
	if err := send.ValidateContentShape(channel, kind, content); err != nil {
		return nil, err
	}
	params, err := send.MarshalParamSpecs(paramSpecsToModel(t.GetParams()))
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}
	return &models.MessageTemplate{
		AppID:    appID,
		Name:     t.GetName(),
		Channel:  int32(channel),
		Kind:     int32(kind),
		Disabled: t.GetDisabled(),
		Params:   params,
		Content:  content,
	}, nil
}

func templateToProto(t *models.MessageTemplate) *pb.TemplateInfo {
	info := &pb.TemplateInfo{
		Id:       t.ID,
		AppId:    t.AppID,
		Name:     t.Name,
		Channel:  pb.TemplateChannel(t.Channel),
		Kind:     pb.TemplateKind(t.Kind),
		Disabled: t.Disabled,
	}
	specs, _ := send.ParseParamSpecs(t.Params)
	info.Params = paramSpecsToProto(specs)
	switch pb.TemplateKind(t.Kind) {
	case pb.TemplateKind_TEMPLATE_KIND_EMAIL_RENDER:
		if c, err := send.DecodeEmailContent(t.Content); err == nil {
			info.Content = &pb.TemplateInfo_Email{Email: &pb.EmailTemplateContent{
				Subject:  c.Subject,
				TextBody: c.TextBody,
				HtmlBody: c.HTMLBody,
			}}
		}
	case pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES:
		if codes, err := send.DecodeVendorCodes(t.Content); err == nil {
			pbCodes := make([]*pb.VendorTemplateCode, 0, len(codes))
			for _, vc := range send.SortVendorCodes(codes) {
				pbCodes = append(pbCodes, &pb.VendorTemplateCode{Vendor: vc.Vendor, TemplateCode: vc.TemplateCode})
			}
			info.Content = &pb.TemplateInfo_VendorCodes{VendorCodes: &pb.SmsVendorCodesContent{Codes: pbCodes}}
		}
	case pb.TemplateKind_TEMPLATE_KIND_SMS_CONTENT:
		if c, err := send.DecodeSmsContent(t.Content); err == nil {
			info.Content = &pb.TemplateInfo_SmsContent{SmsContent: &pb.SmsContentTemplate{Content: c.Content}}
		}
	}
	if !t.CreatedAt.IsZero() {
		info.CreatedAt = t.CreatedAt.Unix()
	}
	if !t.UpdatedAt.IsZero() {
		info.UpdatedAt = t.UpdatedAt.Unix()
	}
	return info
}

func paramSpecsToModel(specs []*pb.TemplateParamSpec) []models.TemplateParamSpec {
	out := make([]models.TemplateParamSpec, len(specs))
	for i, s := range specs {
		out[i] = models.TemplateParamSpec{Name: s.GetName(), Required: s.GetRequired(), Description: s.GetDescription()}
	}
	return out
}

func paramSpecsToProto(specs []models.TemplateParamSpec) []*pb.TemplateParamSpec {
	out := make([]*pb.TemplateParamSpec, len(specs))
	for i, s := range specs {
		out[i] = &pb.TemplateParamSpec{Name: s.Name, Required: s.Required, Description: s.Description}
	}
	return out
}

// mintSecret generates the app secret: "msg_" + 32 bytes base64url.
func mintSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint secret: %w", err)
	}
	return "msg_" + base64.RawURLEncoding.EncodeToString(buf), nil
}
