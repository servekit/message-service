-- phase3-message-tenant-key.sql：③期 T5 —— message-service tenant_key 列化
-- 在 message-service 的 PG 上执行一次（幂等：重复执行无害）。
--
-- 步骤（D-③4 顺序）：加列 → 按映射回填 → 建新索引（名字与 GORM 模型 tag 一致，
-- AutoMigrate 不会重复建）→ 对账（回填行数达标，不等即中止）→ 删旧组合唯一索引。
-- 旧列（app_id）保留到 ④ 期窗口关闭；窗口期代码按 tenant_key 读取、回退规则见
-- models.AppTenantKey（列空 → app_key 字面量，T10 总装清零空值）。
--
-- 语义：
--   message_apps            tenant_key = app_key 字面量（③期映射即 app_key；
--                           T10 总装把存量行重映射到 ten_*）。唯一索引 → 一个
--                           tenant 一行配置（日限额行）。
--   message_templates       app_id=0 → tenant_key 保持 NULL（共享，语义不变）；
--                           其余回填所属 app 的 tenant_key。
--   message_policies        回填所属 app 的 tenant_key；唯一键
--                           (app_id,channel,scene) → (tenant_key,channel,scene)。
--   message_channel_accounts / message_signatures
--                           加可空 tenant_key，NULL = 平台池（存量行为不变，
--                           不回填）。
--
-- 对账口径（控制器输入）：执行前记录 SELECT count(*) FROM message_apps 等基准；
-- apps：count(tenant_key) 必须 = count(*)；templates：app_id<>0 的行必须全部
-- 回填（app_id=0 共享行保持 NULL）；policies：count(tenant_key) 必须 = count(*)。
-- 不满足即 RAISE EXCEPTION 中止整个事务（旧索引未动，可排查后安全重试）。
--
-- 执行：psql "postgres://…/testkit" -v ON_ERROR_STOP=1 -f phase3-message-tenant-key.sql
-- dry-run：将末尾 COMMIT 改为 ROLLBACK。
--
-- 警告：compose 栈若配置表前缀（USER/MSG_*_DB_TABLE_PREFIX），裸表名解析不到
--       目标 —— 执行前先确认 search_path 下的 message_* 就是目标表
--       （dev testkit 栈为无前缀裸表名）。

BEGIN;

-- ── 1. 加列（幂等）────────────────────────────────────────────
ALTER TABLE message_apps             ADD COLUMN IF NOT EXISTS tenant_key VARCHAR(16);
ALTER TABLE message_templates        ADD COLUMN IF NOT EXISTS tenant_key VARCHAR(16);
ALTER TABLE message_policies         ADD COLUMN IF NOT EXISTS tenant_key VARCHAR(16);
ALTER TABLE message_channel_accounts ADD COLUMN IF NOT EXISTS tenant_key VARCHAR(16);
ALTER TABLE message_signatures       ADD COLUMN IF NOT EXISTS tenant_key VARCHAR(16);

-- ── 2. 回填（幂等：仅回填 NULL 行）────────────────────────────
-- apps：映射列 = app_key 字面量（③期目录键即 app_key）。
UPDATE message_apps SET tenant_key = app_key WHERE tenant_key IS NULL;

-- templates：app_id<>0 的行回填所属 app 的映射；app_id=0 保持 NULL（共享）。
UPDATE message_templates t SET tenant_key = a.tenant_key
  FROM message_apps a
  WHERE t.app_id = a.id AND t.app_id <> 0 AND t.tenant_key IS NULL;

-- policies：回填所属 app 的映射。
UPDATE message_policies p SET tenant_key = a.tenant_key
  FROM message_apps a
  WHERE p.app_id = a.id AND p.tenant_key IS NULL;

-- channel_accounts / signatures：不回填 —— NULL 即平台池（③期资源域语义）。

-- ── 3. 建新索引（先建新；名字与 GORM 模型 tag 一致）────────────
-- apps：一个 tenant 一行配置行（Trusted 懒建 + 存量映射共用该唯一性）。
CREATE UNIQUE INDEX IF NOT EXISTS uniq_msg_apps_tenant_key ON message_apps(tenant_key);
-- policies：唯一键 (app_id,channel,scene) → (tenant_key,channel,scene)。
CREATE UNIQUE INDEX IF NOT EXISTS uniq_msg_policy_tenant_ch_scene ON message_policies(tenant_key, channel, scene);

-- ── 4. 对账：不等即中止（旧索引未动，可排查后重跑）──────────────
DO $$
DECLARE
  total bigint;
  filled bigint;
BEGIN
  EXECUTE 'SELECT count(*), count(tenant_key) FROM message_apps' INTO total, filled;
  RAISE NOTICE 'phase3 message tenant_key reconcile message_apps: total=% filled=%', total, filled;
  IF filled <> total THEN
    RAISE EXCEPTION 'phase3 message_apps backfill incomplete: % of % rows filled', filled, total;
  END IF;

  EXECUTE 'SELECT count(*) FILTER (WHERE app_id <> 0), count(tenant_key) FILTER (WHERE app_id <> 0) FROM message_templates' INTO total, filled;
  RAISE NOTICE 'phase3 message tenant_key reconcile message_templates: non_shared=% filled=%', total, filled;
  IF filled <> total THEN
    RAISE EXCEPTION 'phase3 message_templates backfill incomplete: % of % non-shared rows filled (rows referencing a missing message_apps.id keep NULL)', filled, total;
  END IF;

  EXECUTE 'SELECT count(*), count(tenant_key) FROM message_policies' INTO total, filled;
  RAISE NOTICE 'phase3 message tenant_key reconcile message_policies: total=% filled=%', total, filled;
  IF filled <> total THEN
    RAISE EXCEPTION 'phase3 message_policies backfill incomplete: % of % rows filled (rows referencing a missing message_apps.id keep NULL)', filled, total;
  END IF;
END $$;

-- ── 5. 删旧 (app_id,channel,scene) 组合唯一索引（对账通过后才执行）──
DROP INDEX IF EXISTS uniq_msg_policy_app_ch_scene;

COMMIT;
