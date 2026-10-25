package main

// domain 域真实实现：对齐 PHP DomainService + ApplyDomainJob。
//
//   - domainTaskSubmitter 实现 admin.DomainTaskSubmitter：建 async_tasks（domain_action）
//     并 dispatchAfterCommit，worker 异步执行（前端据 ssl_status=pending 显示「配置中」）。
//   - handleApplyDomain 是 JobApplyDomain 的执行体：exec 调 /usr/local/bin/3hub-domain
//     助手（sudoers 受限），解析 stdout JSON，回写 domains 表。
//
// 3hub-domain 助手未安装 / 签发失败时只回写 ssl_status=failed，不抛异常（对齐 PHP
// DomainService「不抛异常，标 failed」的语义，队列不重试、item 标 succeeded）。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strconv"

	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/task"
)

const domainHelperPath = "/usr/local/bin/3hub-domain"

// domainTaskSubmitter 实现 admin.DomainTaskSubmitter。
type domainTaskSubmitter struct {
	async *task.Service
}

// NewDomainTaskSubmitter 构造域名域的异步任务提交器。
func NewDomainTaskSubmitter(async *task.Service) admin.DomainTaskSubmitter {
	return &domainTaskSubmitter{async: async}
}

// Submit 对齐 DomainService::submit：建 domain_action 任务 + dispatchAfterCommit。
func (s *domainTaskSubmitter) Submit(ctx context.Context, action string, domainID uint64, domainName string) (*admin.DomainTask, error) {
	meta, err := json.Marshal(map[string]string{"action": action, "domain": domainName})
	if err != nil {
		return nil, err
	}
	subjectType := "domain"
	subjectID := int64(domainID)
	t, err := s.async.Create(ctx, task.CreateInput{
		Type:        task.TypeDomainAction,
		SubjectType: &subjectType,
		SubjectID:   &subjectID,
		Total:       1,
		Meta:        meta,
		MaxAttempts: 3,
		ItemKeys:    []string{"domain:" + strconv.FormatUint(domainID, 10)},
	})
	if err != nil {
		return nil, err
	}
	if err := s.async.DispatchAfterCommit(ctx, t); err != nil {
		return nil, err
	}
	return &admin.DomainTask{ID: uint64(t.ID)}, nil
}

// registerDomainHandler 注册 JobApplyDomain 的执行体。
func registerDomainHandler(q *task.Queue, db *sql.DB, logger *log.Logger) {
	q.Register(task.JobApplyDomain, func(ctx context.Context, job task.JobPayload) error {
		return handleApplyDomain(ctx, db, logger, job)
	})
}

// handleApplyDomain 执行一次域名操作（对齐 ApplyDomainJob::handle）。
func handleApplyDomain(ctx context.Context, db *sql.DB, logger *log.Logger, job task.JobPayload) error {
	action := job.Action
	if action == "" {
		action = "apply"
	}
	domainID := job.NodeID

	// 读域名行（拿 domain 名；remove 后行已删则跳过）
	var name string
	err := db.QueryRowContext(ctx, "SELECT domain FROM domains WHERE id = ?", domainID).Scan(&name)
	if err == sql.ErrNoRows {
		return nil // 行已删（remove 场景），不回写
	}
	if err != nil {
		return fmt.Errorf("读域名 #%d: %w", domainID, err)
	}
	if job.Domain != nil && *job.Domain != "" {
		name = *job.Domain
	}

	res := runDomainHelper(ctx, action, name)
	if err := writeDomainResult(ctx, db, domainID, res); err != nil {
		logger.Printf("[domain] 回写域名 #%d 结果失败: %v", domainID, err)
	}
	if res.Error != "" {
		logger.Printf("[domain] 域名操作失败 domain=%s action=%s: %s", name, action, res.Error)
	}
	return nil
}

// domainResult 是 3hub-domain 的输出解析结果。
type domainResult struct {
	OK           bool
	SSLStatus    string
	CertExpiresAt *string
	Error        string
}

// runDomainHelper exec 调 3hub-domain 助手并解析 stdout JSON。
func runDomainHelper(ctx context.Context, action, domain string) domainResult {
	var out domainResult
	if _, err := exec.LookPath(domainHelperPath); err != nil {
		out.SSLStatus = "failed"
		out.Error = "3hub-domain 助手未安装（本地开发环境）"
		return out
	}

	cmd := exec.CommandContext(ctx, "sudo", "-n", domainHelperPath, action, domain)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	stderrTail := stderr.String()
	if len(stderrTail) > 800 {
		stderrTail = stderrTail[len(stderrTail)-800:]
	}

	var parsed map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		out.SSLStatus = "failed"
		out.Error = fmt.Sprintf("3hub-domain 输出无法解析（exit=%v）", runErr)
		if stderrTail != "" {
			out.Error += "\n" + stderrTail
		}
		return out
	}

	ok, _ := parsed["ok"].(bool)
	ssl, _ := parsed["ssl_status"].(string)
	if ssl == "" {
		if ok {
			ssl = "ok"
		} else {
			ssl = "failed"
		}
	}
	switch ssl {
	case "none", "pending", "ok", "expiring", "failed":
	default:
		if ok {
			ssl = "ok"
		} else {
			ssl = "failed"
		}
	}
	out.OK = ok
	out.SSLStatus = ssl
	if v, isStr := parsed["cert_expires_at"].(string); isStr && v != "" {
		out.CertExpiresAt = &v
	}
	if v, isStr := parsed["error"].(string); isStr && v != "" {
		out.Error = v
	}
	if !ok {
		if out.Error == "" {
			out.Error = "3hub-domain 执行失败"
		}
		if stderrTail != "" {
			out.Error += "\n" + stderrTail
		}
	}
	return out
}

// writeDomainResult 回写 domains 表（对齐 DomainService::finish）。
func writeDomainResult(ctx context.Context, db *sql.DB, domainID int64, res domainResult) error {
	var expires any
	if res.CertExpiresAt != nil {
		expires = *res.CertExpiresAt
	}
	var lastErr any
	if res.Error != "" {
		lastErr = res.Error
	}
	_, err := db.ExecContext(ctx,
		"UPDATE domains SET ssl_status = ?, cert_expires_at = ?, last_error = ?, updated_at = NOW() WHERE id = ?",
		res.SSLStatus, expires, lastErr, domainID)
	return err
}
