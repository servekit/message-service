-- phase4-drop-legacy.sql：④期 T8 —— 窗口关闭（数据侧）：message-service
-- 在 message-service 的 PG 上执行一次（幂等：重复执行无害）。
--
-- 前置（控制器输入，全部满足才执行本脚本）：
--   1. 代码对账：T7/T8 后代码已无模板/策略 app_id 与 app 表 app_secret
--      的读写（grep 各列名，仅历史注释提及）；
--   2. pg_dump 留档：相关表已备份到 deploy/backups/；
--   3. 行数记录：删前每表 SELECT count(*) 基准已记录。
--
-- 删除清单（spec §9.1.3：凭据列废弃不迁；④ 窗口关闭删旧指针与旧凭据）：
--   message_templates  app_id 列 + idx_message_templates_app_id（tenant_key 已承载归属）
--   message_policies   app_id 列 + uniq_msg_policy_app_ch_scene（③ 已建
--                      uniq_msg_policy_tenant_ch_scene；③ 期脚本对 dev 已删，
--                      此处守卫补删覆盖未跑过 ③ 脚本的环境）
--   message_apps       app_secret 列（表保留 —— 租户配置行/限额载体）
--
-- 对账口径：message_policies 的 count(tenant_key) 必须 = count(*)（③ 回填
-- 完备）；不等即 RAISE EXCEPTION 中止整个事务。message_templates 的
-- tenant_key NULL 行是共享语义（原 app_id=0），不参与对账。
--
-- 备份（执行前手工跑一次，产物不进 git）：
--   pg_dump "postgres://…/testkit" -t message_templates -t message_policies \
--     -t message_apps -Fc -f deploy/backups/phase4-message-pre-drop.dump
--
-- 执行：psql "postgres://…/testkit" -v ON_ERROR_STOP=1 -f phase4-drop-legacy.sql
-- dry-run：将末尾 COMMIT 改为 ROLLBACK。
--
-- 警告：compose 栈若配置 MSG_*_DB_TABLE_PREFIX，裸表名解析不到目标 ——
--       执行前先确认 search_path 下的 message_* 就是目标表
--       （dev testkit 栈为无前缀裸表名）。make migrate 的镜像步骤
--       （postMigrateDropLegacy）做同一件事，先跑哪个都可以。

BEGIN;

-- ── 1. 对账：策略行 tenant_key 回填完备才能删指针────────────────
DO $$
DECLARE
  total bigint;
  filled bigint;
BEGIN
  EXECUTE 'SELECT count(*), count(tenant_key) FROM message_policies' INTO total, filled;
  RAISE NOTICE 'phase4 drop-legacy reconcile message_policies: total=% tenant_key_filled=%', total, filled;
  IF filled <> total THEN
    RAISE EXCEPTION 'phase4 drop-legacy: message_policies has % of % rows without tenant_key; refusing to drop app_id', total - filled, total;
  END IF;
END $$;

-- ── 2. 替换索引在位（守卫）──────────────────────────────────────
DO $$
DECLARE
  exists_ boolean;
BEGIN
  SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'uniq_msg_policy_tenant_ch_scene') INTO exists_;
  IF NOT exists_ THEN
    RAISE EXCEPTION 'phase4 drop-legacy: replacement index uniq_msg_policy_tenant_ch_scene missing; refusing to drop legacy columns';
  END IF;
END $$;

-- ── 3. 删列（幂等；先删列上旧索引再删列）────────────────────────
DROP INDEX IF EXISTS idx_message_templates_app_id;     -- message_templates(app_id)
DROP INDEX IF EXISTS uniq_msg_policy_app_ch_scene;     -- message_policies(app_id,channel,scene)
ALTER TABLE message_templates DROP COLUMN IF EXISTS app_id;
ALTER TABLE message_policies  DROP COLUMN IF EXISTS app_id;
-- message_apps 保留（租户配置行/日限额载体）；仅删凭据列（spec §9.1.3）。
ALTER TABLE message_apps      DROP COLUMN IF EXISTS app_secret;

COMMIT;
