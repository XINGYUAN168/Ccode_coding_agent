package teams

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"ccode/internal/agent"
	"ccode/internal/conversation"
	"ccode/internal/permissions"
	"ccode/internal/planfile"
)

// LeadName 是协调端约定使用的发送者和接收者标识。队员把空闲通知发送给它，并从
// From == LeadName 的消息中读取 Lead 分配的任务。
const LeadName = "lead"

// ShutdownPrefix 把邮箱消息标记为终止队员的请求。Lead 写入此类消息以正常结束成员；
// 运行器在空闲轮询期间看到它后退出循环。
const ShutdownPrefix = "[shutdown]"

// IdlePollInterval 是空闲队员扫描收件箱中新增任务的时间间隔。
const IdlePollInterval = 500 * time.Millisecond

// IsShutdownRequest 通过匹配关闭前缀，报告邮箱消息是否要求队员退出。

// CreateIdleNotification 构造队员完成一轮工作后发送给 Lead 的消息。Lead 读取这些
// 消息来分派工作。
func CreateIdleNotification(memberName, reason string) FileMailMessage {
	return NewFileMailMessage(memberName, fmt.Sprintf("[idle] %s (reason: %s)", memberName, reason))
}

// RunInProcessTeammate 在当前进程中驱动队员的主循环。它会一直阻塞，直到 ctx 被取消
// 或收件箱收到关闭请求。每轮执行以下步骤：
//
// 1. waitForNextPromptOrShutdown——把所有待处理邮箱消息合并成用户提示词（遇到关闭或
// 取消则返回）。2. runAgent——在共享对话上调用 agent.Run，通过 eventOut 转发事件；
// 通道关闭表示本轮结束。3. sendIdleNotification——向 Lead 收件箱写入空闲标记，使其
// 可以分派下一个任务。
//
// 初始提示词用于启动第一轮；后续各轮从邮箱获取提示词。
func RunInProcessTeammate(
	ctx context.Context,
	team *Team,
	member *Member,
	initialPrompt string,
	addendum string,
	eventOut chan<- agent.AgentEvent,
) error {
	if addendum != "" {
		member.Conv.AddSystemReminder(addendum)
	}

	nextPrompt := initialPrompt
	idleReason := "available"

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// 把本轮开始前到达收件箱的消息作为系统提醒合并进对话，让模型将其视为传入
		// 通知而非用户指令。
		if reminder := InjectPendingMessages(team, member.Name); reminder != "" {
			member.Conv.AddSystemReminder(reminder)
		}

		if nextPrompt != "" {
			member.Conv.AddUserMessage(nextPrompt)
		}
		nextPrompt = ""

		ch := member.AgentRef.Run(ctx, member.Conv)
		for ev := range ch {
			// 更新进度追踪。
			if member.Progress != nil {
				switch e := ev.(type) {
				case agent.ToolUseEvent:
					member.Progress.RecordToolUse(e.ToolName, e.Args)
				case agent.UsageEvent:
					member.Progress.RecordTokens(int64(e.InputTokens), int64(e.OutputTokens))
				}
			}
			if eventOut != nil {
				select {
				case eventOut <- ev:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if e, ok := ev.(agent.ErrorEvent); ok && e.Message != "" {
				idleReason = "failed"
			}
		}

		if member.Progress != nil {
			if idleReason == "failed" {
				member.Progress.SetStatus("failed")
			} else {
				member.Progress.SetStatus("idle")
			}
		}

		// 计划模式的队友：一轮跑完意味着它调了 ExitPlanMode，计划已经落到磁盘。
		// 把计划交给 Lead 审批，通过了才解除只读限制开始动手。
		if planModeActive(member) {
			approved, feedback, err := requestPlanApproval(ctx, team, member)
			if err != nil {
				return err
			}
			if approved {
				// 批准后切回正常权限，队友可以改文件了
				member.AgentRef.Checker.Mode = permissions.ModeDefault
				nextPrompt = "Lead 已批准你的计划，现在按计划开始执行。"
			} else {
				// 驳回时留在计划模式，带着修改意见重写计划
				nextPrompt = "Lead 驳回了你的计划，修改意见：" + feedback + "\n请据此修订计划后再次提交。"
			}
			continue
		}

		// 通知 Lead 此队员已完成本轮，使 Lead 可以决定是否继续分配工作。
		_ = team.MailBox.Send(LeadName, CreateIdleNotification(member.Name, idleReason))
		idleReason = "available"

		// 空闲轮询：等待 IdlePollInterval 后排空收件箱。遇到关闭消息则停止，否则构造
		// 下一条提示词并重新进入循环。
		prompt, shutdown, err := waitForNextPromptOrShutdown(ctx, team, member.Name)
		if err != nil {
			return err
		}
		if shutdown != nil {
			// 收工前先给 Lead 一个明确答复，让它知道可以回收窗格了。
			// 队友这里一律同意：它已经处在空闲轮询里，手上没有干到一半的活。
			// 真正需要拒绝的场景是干活干到一半被打断，那种情况下队友根本轮询不到这条消息。
			if shutdown.Type == MsgShutdownRequest {
				_ = team.MailBox.Send(LeadName,
					NewShutdownResponse(member.Name, shutdown.RequestID, true, "acknowledged, shutting down"))
			}
			return nil
		}
		nextPrompt = prompt
	}
}

// waitForNextPromptOrShutdown 阻塞到收件箱至少有一条消息，再把整批未读消息转换成
// 下一条用户提示词。如果其中有关闭请求，该函数不构造提示词，直接返回关闭消息。
// planModeActive 判断队友是否处在计划模式。只有被 Lead 标了 planModeRequired
// 的队友才会进这个模式，普通队友直接干活。
func planModeActive(member *Member) bool {
	return member.AgentRef != nil &&
		member.AgentRef.Checker != nil &&
		member.AgentRef.Checker.Mode == permissions.ModePlan
}

// requestPlanApproval 把队友写好的计划发给 Lead，然后阻塞等待批复。
//
// 队友这时候手上是只读权限，等多久都不会造成破坏，所以这里不设超时：
// 与其超时后自作主张开始改文件，不如一直等着，由用户从 Lead 那边推进。
func requestPlanApproval(ctx context.Context, team *Team, member *Member) (bool, string, error) {
	plan := readPlanForReview(member)
	req := NewPlanApprovalRequest(member.Name, plan)
	if err := team.MailBox.Send(LeadName, req); err != nil {
		return false, "", err
	}
	if member.Progress != nil {
		member.Progress.SetStatus("awaiting plan approval")
	}

	for {
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(IdlePollInterval):
		}

		msgs, err := team.MailBox.ReadUnread(member.Name)
		if err != nil {
			return false, "", err
		}
		for _, m := range msgs {
			// 只认对应这次请求的批复，别的消息留到下一轮再处理
			if m.Type == MsgPlanApprovalResponse && m.RequestID == req.RequestID {
				_ = team.MailBox.MarkAllRead(member.Name)
				return m.Approved(), m.Text, nil
			}
		}
	}
}

// readPlanForReview 读出队友写好的计划全文，交给 Lead 审阅。
func readPlanForReview(member *Member) string {
	workDir := ""
	if member.AgentRef != nil {
		workDir = member.AgentRef.WorkDir
	}
	path := planfile.GetOrCreatePlanPath(workDir)
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return "（计划文件为空，队友可能未按要求写入计划）"
	}
	return string(data)
}

func waitForNextPromptOrShutdown(ctx context.Context, team *Team, memberName string) (string, *FileMailMessage, error) {
	for {
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(IdlePollInterval):
		}

		msgs, err := team.MailBox.ReadUnread(memberName)
		if err != nil {
			return "", nil, err
		}
		if len(msgs) == 0 {
			continue
		}

		var shutdown *FileMailMessage
		var keep []FileMailMessage
		for i, m := range msgs {
			if IsShutdownRequest(m) {
				shutdown = &msgs[i]
				continue
			}
			keep = append(keep, m)
		}
		_ = team.MailBox.MarkAllRead(memberName)

		if shutdown != nil {
			return "", shutdown, nil
		}
		return formatInboundAsPrompt(keep), nil, nil
	}
}

// DrainLeadMailbox 读取每个团队 Lead 收件箱中的全部未读通知，并按每个团队一条
// system-reminder 字符串返回。Lead 的主循环把此函数设置为 Agent.NotificationFn，
// 使队员空闲通知在每轮开始时呈现给模型。
func DrainLeadMailbox(mgr *TeamManager) []string {
	if mgr == nil {
		return nil
	}
	var notes []string
	for _, name := range mgr.ListTeams() {
		team := mgr.GetTeam(name)
		if team == nil {
			continue
		}
		msgs, err := team.MailBox.ReadUnread(LeadName)
		if err != nil || len(msgs) == 0 {
			continue
		}
		var sb strings.Builder
		sb.WriteString("<team-notification team=\"")
		sb.WriteString(name)
		sb.WriteString("\">\n")
		for _, m := range msgs {
			sb.WriteString("from=")
			sb.WriteString(m.From)
			sb.WriteString(": ")
			sb.WriteString(m.Text)
			sb.WriteString("\n")
		}
		sb.WriteString("</team-notification>")
		notes = append(notes, sb.String())
		_ = team.MailBox.MarkAllRead(LeadName)
	}
	return notes
}

// formatInboundAsPrompt 把一批未读消息转换成一条用户提示词。每条消息都标记发送者，
// 使队员可以正确回复；格式与队员消息一致，但使用纯文本而非 XML。
func formatInboundAsPrompt(msgs []FileMailMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("You have new messages from your team:\n\n")
	for _, m := range msgs {
		sb.WriteString(fmt.Sprintf("From %s: %s\n\n", m.From, m.Text))
	}
	return sb.String()
}

// _ 在仅通过 Member.Conv 方法间接引用 conversation 时消除未使用导入警告。
var _ = conversation.NewManager
