package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golem_plugin_hermes/internal/domain"

	"github.com/sbgayhub/golem/sdk/contact"
	"github.com/sbgayhub/golem/sdk/message"
	"github.com/sbgayhub/golem/sdk/plugin"
)

const hermesCommandHelp = `Hermes 微信命令（仅机器人所有者可用）

/hermes help                 查看本帮助
/hermes status               查看当前 Hermes 会话、模型和上下文状态
/hermes new                  创建全新会话
/hermes reset                创建全新会话（new 的别名）
/hermes approve              单次批准当前确认请求
/hermes always               批准当前重置，并以后不再询问
/hermes cancel               取消当前重置确认请求
/hermes personality [名称]   查看或切换 Hermes 人格
/hermes observations status  查看 Observation V2 队列和阻断状态
/hermes observations repair current  校验并重排当前会话最早的阻断批次
/hermes observations prune 30        清理 30 天前已确认的旧记录（保留序号水位）

这些命令使用当前微信聊天对应的 Hermes 会话。群聊命令仍由 Owner 执行，回复发送到当前群聊。`

var forwardedHermesCommands = map[string]bool{
	"status":      false,
	"new":         false,
	"reset":       false,
	"approve":     false,
	"always":      false,
	"cancel":      false,
	"personality": true,
}

type hermesCommand struct {
	_       struct{} `cmd:"hermes" help:"通过微信操作 Hermes（仅机器人所有者可用）" usage:"/hermes <命令> [参数...]" example:"/hermes help\n/hermes status\n/hermes reset\n/hermes personality technical"`
	Input   string   `arg:"命令" help:"help、status、new、reset、approve、always、cancel、personality 或 observations" variadic:"true"`
	Context *plugin.Command
}

func (p *HermesPlugin) GetCommands() []string {
	if p.commands == nil {
		return nil
	}
	return p.commands.Commands()
}

func (p *HermesPlugin) GetCommandSchemas() []*plugin.CommandSchema {
	if p.commands == nil {
		return nil
	}
	return p.commands.Schemas()
}

func (p *HermesPlugin) OnCommand(command *plugin.Command) (string, error) {
	if p.commandErr != nil {
		return "", fmt.Errorf("Hermes 命令不可用: %w", p.commandErr)
	}
	if p.commands == nil {
		return "", errors.New("Hermes 命令尚未初始化")
	}
	return p.commands.Dispatch(command)
}

func (p *HermesPlugin) handleHermesCommand(value hermesCommand) (string, error) {
	ownerID, sender, isChatroom, err := p.authorizeHermesCommand(value.Context)
	if err != nil {
		return "", err
	}

	input := strings.TrimSpace(value.Input)
	if input == "" || strings.EqualFold(strings.TrimPrefix(input, "/"), "help") {
		return hermesCommandHelp, nil
	}
	if handled, result, maintenanceErr := p.handleObservationMaintenance(value.Context, input); handled {
		return result, maintenanceErr
	}
	forwarded, err := normalizeHermesCommand(input)
	if err != nil {
		return "", err
	}
	event, err := newHermesCommandEvent(value.Context, ownerID, sender, isChatroom, forwarded)
	if err != nil {
		return "", err
	}
	if preemptsHermesSession(forwarded) {
		if err := p.preemptHermesSession(event.SessionID); err != nil {
			return "", err
		}
	}
	if err := p.acceptInbox(event); err != nil {
		return "", err
	}
	return "", nil
}

func (p *HermesPlugin) handleObservationMaintenance(
	command *plugin.Command,
	input string,
) (bool, string, error) {
	fields := strings.Fields(input)
	if len(fields) == 0 || !strings.EqualFold(strings.TrimPrefix(fields[0], "/"), "observations") {
		return false, "", nil
	}
	if len(input) > 512 || strings.IndexFunc(input, unicode.IsControl) >= 0 {
		return true, "", errors.New("observations 命令包含非法字符或长度超过限制")
	}
	if len(fields) < 2 {
		return true, "", errors.New("缺少 observations 子命令；可用 status、repair 或 prune")
	}
	p.lifecycleMu.Lock()
	store := p.store
	p.lifecycleMu.Unlock()
	if store == nil {
		return true, "", errors.New("Hermes Kernel 尚未启动")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	switch strings.ToLower(fields[1]) {
	case "status":
		if len(fields) != 2 {
			return true, "", errors.New("/hermes observations status 不接受参数")
		}
		status, err := store.ObservationMaintenanceStatus(ctx)
		if err != nil {
			return true, "", fmt.Errorf("读取 Observation V2 状态: %w", err)
		}
		oldest := "无"
		if !status.OldestTerminalUpdated.IsZero() {
			oldest = status.OldestTerminalUpdated.Format(time.RFC3339)
		}
		result := fmt.Sprintf("Observation V2：pending=%d leased=%d retry_wait=%d acked=%d conflict=%d dead_letter=%d；阻断会话=%d，最早阻断=%s",
			status.Pending, status.Leased, status.RetryWait, status.Acked, status.Conflict,
			status.DeadLetter, status.BlockedConversations, oldest)
		for _, blockage := range status.Blocked {
			result += fmt.Sprintf("\n- %s seq=%d state=%s error=%s", blockage.ConversationID,
				blockage.ConversationSeq, blockage.State, observationMaintenanceError(blockage.LastError))
		}
		return true, result, nil
	case "repair", "requeue":
		if len(fields) != 3 {
			return true, "", errors.New("用法：/hermes observations repair <current|conversation_id>")
		}
		conversationID, err := observationMaintenanceConversation(command, fields[2])
		if err != nil {
			return true, "", err
		}
		count, err := store.RepairObservationConversation(ctx, conversationID, time.Now())
		if err != nil {
			return true, "", fmt.Errorf("修复 Observation V2 会话 %s: %w", conversationID, err)
		}
		return true, fmt.Sprintf("已校验并重排会话 %s 的 %d 条阻断 observation；未修改 payload，未跳过序号。", conversationID, count), nil
	case "prune":
		if len(fields) != 3 {
			return true, "", errors.New("用法：/hermes observations prune <保留天数>")
		}
		days, err := strconv.Atoi(fields[2])
		if err != nil || days < 1 || days > 3650 {
			return true, "", errors.New("Observation 保留天数必须在 1 到 3650 之间")
		}
		count, err := store.PruneAckedObservations(ctx, time.Now().Add(-time.Duration(days)*24*time.Hour), 1000)
		if err != nil {
			return true, "", fmt.Errorf("清理 Observation V2 已确认记录: %w", err)
		}
		return true, fmt.Sprintf("已清理 %d 条过期 acked observation；每个会话至少保留最新序号水位，未清理阻断或未确认记录。", count), nil
	default:
		return true, "", fmt.Errorf("不支持的 observations 子命令 %q；可用 status、repair 或 prune", fields[1])
	}
}

func observationMaintenanceError(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 160 {
		value = string(runes[:160]) + "…"
	}
	if value == "" {
		return "无"
	}
	return value
}

func observationMaintenanceConversation(command *plugin.Command, value string) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.EqualFold(value, "current") {
		if !strings.HasPrefix(value, "wechat:") || strings.IndexFunc(value, unicode.IsControl) >= 0 || len(value) > 512 {
			return "", errors.New("conversation_id 无效；使用 current 或 status 输出中的 wechat 会话 ID")
		}
		return value, nil
	}
	if command == nil || command.GetSender() == nil {
		return "", errors.New("无法识别当前微信会话")
	}
	sender := command.GetSender()
	sessionID := "private:" + strings.TrimSpace(sender.GetUsername())
	if sender.GetType() == contact.ContactType_CONTACT_TYPE_CHATROOM {
		sessionID = "chatroom:" + strings.TrimSpace(sender.GetUsername())
	}
	if strings.HasSuffix(sessionID, ":") {
		return "", errors.New("当前微信会话 ID 为空")
	}
	return domain.StableConversationID(domain.ChannelBinding{Channel: "wechat", SessionID: sessionID}), nil
}

func (p *HermesPlugin) authorizeHermesCommand(command *plugin.Command) (string, *contact.Contact, bool, error) {
	if command == nil || command.GetSender() == nil {
		return "", nil, false, errors.New("无法识别命令发送者")
	}
	p.refreshIdentity()
	p.identityMu.RLock()
	ownerID := strings.TrimSpace(p.ownerID)
	p.identityMu.RUnlock()
	if ownerID == "" {
		return "", nil, false, errors.New("未配置机器人所有者，已拒绝 Hermes 命令；请先设置 Host 的 owner")
	}

	sender := command.GetSender()
	senderID := strings.TrimSpace(sender.GetUsername())
	if senderID == "" {
		return "", nil, false, errors.New("命令发送者 wxId 为空")
	}
	isChatroom := sender.GetType() == contact.ContactType_CONTACT_TYPE_CHATROOM
	if !isChatroom && senderID != ownerID {
		return "", nil, false, errors.New("该命令仅允许机器人所有者执行")
	}
	// 群成员信息没有包含在 Command RPC 中。Host 在进入插件 RPC 前已使用
	// Message.Member 完成 Owner 校验，因此这里只信任同进程 Host 的鉴权结果。
	return ownerID, sender, isChatroom, nil
}

func normalizeHermesCommand(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", errors.New("缺少 Hermes 命令；发送 /hermes help 查看用法")
	}
	if len(input) > 512 || strings.IndexFunc(input, unicode.IsControl) >= 0 {
		return "", errors.New("Hermes 命令包含非法字符或长度超过限制")
	}
	fields := strings.Fields(input)
	name := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	allowsArguments, supported := forwardedHermesCommands[name]
	if !supported || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("不支持的 Hermes 命令 %q；发送 /hermes help 查看白名单", fields[0])
	}
	if len(fields) > 1 && !allowsArguments {
		return "", fmt.Errorf("Hermes 命令 /%s 不接受参数", name)
	}
	return "/" + strings.Join(append([]string{name}, fields[1:]...), " "), nil
}

func newHermesCommandEvent(
	command *plugin.Command,
	ownerID string,
	sender *contact.Contact,
	isChatroom bool,
	forwarded string,
) (domain.InboxEvent, error) {
	randomID, err := randomCommandID()
	if err != nil {
		return domain.InboxEvent{}, fmt.Errorf("生成 Hermes 命令事件 ID: %w", err)
	}
	now := time.Now()
	senderID := strings.TrimSpace(sender.GetUsername())
	sessionID := "private:" + senderID
	principalName := displayContact(sender)
	roomName := ""
	if isChatroom {
		sessionID = "chatroom:" + senderID
		// Command RPC 只包含群联系人，不包含命令发送成员。保留 owner ID
		// 用于鉴权和微信提醒，但不能将它作为昵称展示。
		principalName = ""
		roomName = displayContact(sender)
	}
	raw := strings.TrimSpace(command.GetRaw())
	if raw == "" {
		raw = "/hermes " + strings.TrimPrefix(forwarded, "/")
	}
	incoming := domain.InboundMessage{
		Text:          raw,
		HermesCommand: forwarded,
		IsChatroom:    isChatroom,
		Mentioned:     isChatroom,
		SpeakerID:     ownerID,
		SpeakerName:   principalName,
		RoomName:      roomName,
		OccurredAt:    now,
	}
	payload, err := json.Marshal(incoming)
	if err != nil {
		return domain.InboxEvent{}, fmt.Errorf("编码 Hermes 命令事件: %w", err)
	}
	return domain.InboxEvent{
		ID:         "event_wechat_command_" + randomID,
		DedupeKey:  "wechat/command/" + randomID,
		Topic:      message.TypeText.Topic,
		SessionID:  sessionID,
		OccurredAt: now,
		Binding: domain.ChannelBinding{
			Channel:    "wechat",
			SessionID:  sessionID,
			ReceiverID: senderID,
			Principal: domain.Principal{
				ID:      ownerID,
				Name:    principalName,
				IsOwner: true,
			},
		},
		Payload: payload,
	}, nil
}

func randomCommandID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (p *HermesPlugin) acceptInbox(event domain.InboxEvent) error {
	p.lifecycleMu.Lock()
	store := p.store
	processor := p.processor
	manager := p.config
	p.lifecycleMu.Unlock()
	if store == nil || processor == nil || manager == nil {
		return errors.New("Hermes Kernel 尚未启动")
	}
	snapshot := manager.Current()
	if snapshot == nil {
		return errors.New("Hermes 配置尚未就绪")
	}
	timeout := time.Duration(snapshot.Ingress.DurableAcceptTimeoutMilliseconds) * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, _, err := store.AcceptInbox(ctx, event); err != nil {
		return fmt.Errorf("持久化 Hermes Inbox: %w", err)
	}
	processor.Notify()
	return nil
}
