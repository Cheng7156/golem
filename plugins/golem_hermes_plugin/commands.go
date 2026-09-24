package main

import (
	"context"
	"fmt"
	"strings"
)

// WhoamiCommand /ghp whoami
type WhoamiCommand struct {
	_ struct{} `cmd:"ghp whoami" help:"核对主人识别配置" usage:"/ghp whoami" example:"/ghp whoami"`
}

func (p *hermesPlugin) handleWhoami(cmd WhoamiCommand) (string, error) {
	rows := []string{
		"🪪 主人识别核对",
		fmt.Sprintf("机器人: %s (%s)", p.self.GetNickname(), p.self.GetUsername()),
	}
	if p.owner != nil && p.owner.GetUsername() != "" {
		rows = append(rows, fmt.Sprintf("宿主Owner: %s (%s) ✅已并入",
			displayContact(p.owner), p.owner.GetUsername()))
	} else {
		rows = append(rows, "宿主Owner: 未配置（请在 data/config.toml 配置 owner，或用 owner_wxids 追加）")
	}
	if len(p.Config.OwnerWxIDs) > 0 {
		rows = append(rows, "配置追加主人: "+strings.Join(p.Config.OwnerWxIDs, ", "))
	}
	rows = append(rows, fmt.Sprintf("识别集共 %d 个wxid；owner_only_private=%v；owner_bypass_ambient=%v；group_sessions_per_user=%v",
		len(p.ownerSet), p.Config.OwnerOnlyPrivate, p.Config.OwnerBypassAmbient, p.Config.GroupSessionsPerUser))
	return joinLines(rows), nil
}

// StatusCommand /ghp status
type StatusCommand struct {
	_ struct{} `cmd:"ghp status" help:"查看 Hermes 插件状态" usage:"/ghp status" example:"/ghp status"`
}

func (p *hermesPlugin) handleStatus(cmd StatusCommand) (string, error) {
	health := "未知"
	if p.eng != nil {
		if err := p.eng.Health(context.Background()); err == nil {
			health = "✅ 在线"
		} else {
			health = "❌ 离线 (" + err.Error() + ")"
		}
	}
	var active int
	if p.disp != nil {
		active = len(p.disp.activeCopy())
	}
	var prompt, completion int
	if p.store != nil {
		prompt, completion = p.store.usageToday()
	}
	total, tagged := p.store.stickerCounts()
	budget := "不限"
	if p.Config.DailyTokenCap > 0 {
		pct := 100 * (prompt + completion) / p.Config.DailyTokenCap
		budget = fmt.Sprintf("%d/%d (%d%%)", prompt+completion, p.Config.DailyTokenCap, pct)
	}
	return fmt.Sprintf(
		"🤖 %s v%s\n"+
			"Gateway: %s [%s]\n"+
			"通道: fast=%s full=%s\n"+
			"活跃会话: %d\n"+
			"今日用量: %s\n"+
			"表情库: %d 张（已打标 %d）\n"+
			"上下文窗口: %d 条/群",
		pluginName, pluginVersion, p.Config.BaseURL, health,
		p.Config.FastProfile, p.Config.FullProfile,
		active, budget, total, tagged, p.Config.ContextWindow,
	), nil
}

// StickerCommand /ghp sticker <count|retag>
type StickerCommand struct {
	_      struct{} `cmd:"ghp sticker" help:"表情库管理" usage:"/ghp sticker <count|retag>" example:"/ghp sticker retag"`
	Action string   `arg:"action" help:"count=统计 retag=重新打标" required:"true"`
}

func (p *hermesPlugin) handleSticker(cmd StickerCommand) (string, error) {
	if !p.Config.StickerEnabled {
		return "表情功能未启用", nil
	}
	switch cmd.Action {
	case "count":
		total, tagged := p.store.stickerCounts()
		rows := []string{fmt.Sprintf("表情库: %d 张，已打标 %d 张", total, tagged)}
		if p.store.db != nil {
			p.store.mu.Lock()
			rs, err := p.store.db.Query("SELECT emotion, COUNT(*) FROM stickers WHERE emotion != '' GROUP BY emotion ORDER BY 2 DESC")
			if err == nil {
				for rs.Next() {
					var em string
					var n int
					if rs.Scan(&em, &n) == nil {
						rows = append(rows, fmt.Sprintf("  %s: %d", em, n))
					}
				}
				rs.Close()
			}
			p.store.mu.Unlock()
		}
		return joinLines(rows), nil
	case "retag":
		if p.store.db == nil {
			return "存储不可用", nil
		}
		p.store.mu.Lock()
		_, err := p.store.db.Exec("UPDATE stickers SET attempts=0 WHERE attempts>=3")
		p.store.mu.Unlock()
		if err != nil {
			return "重置失败: " + err.Error(), nil
		}
		go p.tagger.tagBatch()
		return "已重置打标计数，后台将重新分类", nil
	default:
		return "未知操作，支持: count / retag", nil
	}
}

func joinLines(rows []string) string {
	out := ""
	for i, r := range rows {
		if i > 0 {
			out += "\n"
		}
		out += r
	}
	return out
}
