// Credentials conversion: proto ChannelAccountCredentials oneof ↔ the
// stored config JSON (provesms/provemail AccountConfig shapes) ↔ the
// masked echo for list/read responses.
package admin

import (
	pb "github.com/servekit/api/gen/go/messaging/v1"
	"github.com/servekit/go-common/jsonx"
	provemail "github.com/servekit/message-service/internal/provider/email"
	provesms "github.com/servekit/message-service/internal/provider/sms"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"
)

// credentialsToModel converts the proto credentials oneof into the stored
// config JSON plus the derived vendor enum (int32: SmsVendor or EmailVendor
// depending on channel) and channel. Exactly one arm must be set
// (protovalidated).
func credentialsToModel(name string, c *pb.ChannelAccountCredentials) (models.RawJSON, int32, pb.TemplateChannel, error) {
	if c == nil {
		return nil, 0, 0, xcodes.ErrBadRequest.New("credentials are required")
	}
	switch arm := c.GetCredentials().(type) {
	case *pb.ChannelAccountCredentials_AliyunSms:
		cfg := arm.AliyunSms
		ac := &provesms.AccountConfig{Name: name, Aliyun: &provesms.AliyunConfig{
			AccessKeyID:     cfg.GetAccessKeyId(),
			AccessKeySecret: cfg.GetAccessKeySecret(),
			RegionID:        cfg.GetRegionId(),
		}}
		return marshalConfig(ac), int32(pb.SmsVendor_SMS_VENDOR_ALIYUN), pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, nil
	case *pb.ChannelAccountCredentials_TencentSms:
		cfg := arm.TencentSms
		ac := &provesms.AccountConfig{Name: name, Tencent: &provesms.TencentConfig{
			SecretID:   cfg.GetSecretId(),
			SecretKey:  cfg.GetSecretKey(),
			SmsSdkAppID: cfg.GetSmsSdkAppId(),
			Region:     cfg.GetRegion(),
			Endpoint:   cfg.GetEndpoint(),
		}}
		return marshalConfig(ac), int32(pb.SmsVendor_SMS_VENDOR_TENCENT), pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, nil
	case *pb.ChannelAccountCredentials_VolcengineSms:
		cfg := arm.VolcengineSms
		ac := &provesms.AccountConfig{Name: name, Volcengine: &provesms.VolcengineConfig{
			AccessKID:  cfg.GetAccessKey(),
			SecretKey:  cfg.GetSecretKey(),
			SmsAccount: cfg.GetSmsAccount(),
			Region:     cfg.GetRegion(),
		}}
		return marshalConfig(ac), int32(pb.SmsVendor_SMS_VENDOR_VOLCENGINE), pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, nil
	case *pb.ChannelAccountCredentials_ByteplusSms:
		cfg := arm.ByteplusSms
		ac := &provesms.AccountConfig{Name: name, Byteplus: &provesms.ByteplusConfig{
			AccessKID:  cfg.GetAccessKey(),
			SecretKey:  cfg.GetSecretKey(),
			SmsAccount: cfg.GetSmsAccount(),
			Region:     cfg.GetRegion(),
		}}
		return marshalConfig(ac), int32(pb.SmsVendor_SMS_VENDOR_BYTEPLUS), pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, nil
	case *pb.ChannelAccountCredentials_HuaweiSms:
		cfg := arm.HuaweiSms
		ac := &provesms.AccountConfig{Name: name, Huawei: &provesms.HuaweiConfig{
			AppKey:   cfg.GetAppKey(),
			AppSecret: cfg.GetAppSecret(),
			Sign:     cfg.GetSign(),
			Endpoint: cfg.GetEndpoint(),
			Region:   cfg.GetRegion(),
		}}
		return marshalConfig(ac), int32(pb.SmsVendor_SMS_VENDOR_HUAWEI), pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, nil
	case *pb.ChannelAccountCredentials_Smtp:
		cfg := arm.Smtp
		ac := &provemail.AccountConfig{
			Name:     name,
			Vendor:   emailVendorToName(cfg.GetBrand()),
			Host:     cfg.GetHost(),
			Port:     int(cfg.GetPort()),
			Username: cfg.GetUsername(),
			Password: cfg.GetPassword(),
			From:     cfg.GetFromAddress(),
		}
		return marshalConfig(ac), int32(cfg.GetBrand()), pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL, nil
	default:
		return nil, 0, 0, xcodes.ErrBadRequest.New("credentials: exactly one vendor arm must be set")
	}
}

// marshalConfig never fails for these plain structs; a failure would be a
// programming error and is surfaced as ErrInternal.
func marshalConfig(v any) models.RawJSON {
	b, err := jsonx.Marshal(v)
	if err != nil {
		return models.RawJSON("{}")
	}
	return b
}

// emailVendorToName maps the brand enum to the config string form accepted
// by the email provider package.
func emailVendorToName(v pb.EmailVendor) string {
	switch v {
	case pb.EmailVendor_EMAIL_VENDOR_ALIYUN:
		return "aliyun"
	case pb.EmailVendor_EMAIL_VENDOR_TENCENT:
		return "tencent"
	case pb.EmailVendor_EMAIL_VENDOR_NETEASE:
		return "netease"
	default:
		return ""
	}
}

// emailVendorFromName is the reverse of emailVendorToName.
func emailVendorFromName(name string) pb.EmailVendor {
	switch name {
	case "aliyun":
		return pb.EmailVendor_EMAIL_VENDOR_ALIYUN
	case "tencent":
		return pb.EmailVendor_EMAIL_VENDOR_TENCENT
	case "netease":
		return pb.EmailVendor_EMAIL_VENDOR_NETEASE
	default:
		return pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED
	}
}

// smsAccountToProtoCredentials echoes an SMS account config with ALL
// credential fields emptied (write-only secrets).
func smsAccountToProtoCredentials(ac *provesms.AccountConfig) *pb.ChannelAccountCredentials {
	out := &pb.ChannelAccountCredentials{}
	if ac.Aliyun != nil {
		out.Credentials = &pb.ChannelAccountCredentials_AliyunSms{AliyunSms: &pb.AliyunSmsCredentials{
			RegionId: ac.Aliyun.RegionID,
		}}
	} else if ac.Tencent != nil {
		out.Credentials = &pb.ChannelAccountCredentials_TencentSms{TencentSms: &pb.TencentSmsCredentials{
			Region:   ac.Tencent.Region,
			Endpoint: ac.Tencent.Endpoint,
		}}
	} else if ac.Volcengine != nil {
		out.Credentials = &pb.ChannelAccountCredentials_VolcengineSms{VolcengineSms: &pb.VolcengineSmsCredentials{
			Region: ac.Volcengine.Region,
		}}
	} else if ac.Byteplus != nil {
		out.Credentials = &pb.ChannelAccountCredentials_ByteplusSms{ByteplusSms: &pb.ByteplusSmsCredentials{
			Region: ac.Byteplus.Region,
		}}
	} else if ac.Huawei != nil {
		out.Credentials = &pb.ChannelAccountCredentials_HuaweiSms{HuaweiSms: &pb.HuaweiSmsCredentials{
			Sign:     ac.Huawei.Sign,
			Endpoint: ac.Huawei.Endpoint,
			Region:   ac.Huawei.Region,
		}}
	}
	return out
}

// emailAccountToProtoCredentials echoes an SMTP account config with the
// password emptied.
func emailAccountToProtoCredentials(ac *provemail.AccountConfig) *pb.ChannelAccountCredentials {
	return &pb.ChannelAccountCredentials{Credentials: &pb.ChannelAccountCredentials_Smtp{Smtp: &pb.SmtpEmailCredentials{
		Brand:       emailVendorFromName(ac.Vendor),
		Host:        ac.Host,
		Port:        int32(ac.Port),
		Username:    ac.Username,
		FromAddress: ac.From,
	}}}
}
