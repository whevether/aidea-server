package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/mylxsw/aidea-server/pkg/ai/chat"
	repo "github.com/mylxsw/aidea-server/pkg/repo"
	"github.com/mylxsw/aidea-server/pkg/service"
	"time"

	"github.com/hibiken/asynq"
	"github.com/mylxsw/aidea-server/config"
	"github.com/mylxsw/aidea-server/internal/coins"
	"github.com/mylxsw/asteria/log"
	"github.com/mylxsw/go-utils/ternary"
)

type GroupChatPayload struct {
	ID              string        `json:"id,omitempty"`
	GroupID         int64         `json:"group_id,omitempty"`
	UserID          int64         `json:"user_id,omitempty"`
	MemberID        int64         `json:"member_id,omitempty"`
	QuestionID      int64         `json:"question_id,omitempty"`
	MessageID       int64         `json:"message_id,omitempty"`
	ModelID         string        `json:"model_id,omitempty"`
	ContextMessages chat.Messages `json:"context_messages,omitempty"`
	CreatedAt       time.Time     `json:"created_at,omitempty"`
	FreezedCoins    int64         `json:"freezed_coins,omitempty"`
}

func (payload *GroupChatPayload) GetTitle() string {
	return "群聊"
}

func (payload *GroupChatPayload) SetID(id string) {
	payload.ID = id
}

func (payload *GroupChatPayload) GetID() string {
	return payload.ID
}

func (payload *GroupChatPayload) GetUID() int64 {
	return payload.UserID
}

func (payload *GroupChatPayload) GetQuotaID() int64 {
	return 0
}

func (payload *GroupChatPayload) GetQuota() int64 {
	return 0
}

func NewGroupChatTask(payload any) *asynq.Task {
	data, _ := json.Marshal(payload)
	return asynq.NewTask(TypeGroupChat, data)
}

func BuildGroupChatHandler(conf *config.Config, ct chat.Chat, rep *repo.Repository, svc *service.Service) TaskHandler {
	return func(ctx context.Context, task *asynq.Task) (err error) {
		var payload GroupChatPayload
		if err := json.Unmarshal(task.Payload(), &payload); err != nil {
			return err
		}

		// 如果任务是 15 分钟前创建的，不再处理
		if payload.CreatedAt.Add(15 * time.Minute).Before(time.Now()) {
			return nil
		}

		defer func() {
			if err2 := recover(); err2 != nil {
				log.With(task).Errorf("panic: %v", err2)
				err = err2.(error)
			}
			if err != nil {
				// 更新消息状态为失败
				msg := repo.ChatGroupMessageUpdate{
					Message: err.Error(),
					Status:  repo.MessageStatusFailed,
					Error:   err.Error(),
				}
				if err := rep.ChatGroup.UpdateChatMessage(ctx, payload.GroupID, payload.UserID, payload.MessageID, msg); err != nil {
					log.With(task).Errorf("update chat message failed: %s", err)
				}

				// 更新队列状态为失败
				if err := rep.Queue.Update(
					context.TODO(),
					payload.GetID(),
					repo.QueueTaskStatusFailed,
					ErrorResult{
						Errors: []string{err.Error()},
					},
				); err != nil {
					log.With(task).Errorf("update queue status failed: %s", err)
				}
			}

			// 无论如何，都要释放用户被冻结的智慧果
			if payload.FreezedCoins > 0 {
				if err := svc.User.UnfreezeUserQuota(ctx, payload.UserID, payload.FreezedCoins); err != nil {
					log.F(log.M{"payload": payload}).Errorf("群聊任务执行失败，释放用户冻结的智慧果失败: %s", err)
				}
			}
		}()

		mod := svc.Chat.Model(ctx, payload.ModelID)
		if mod == nil || mod.Status == repo.ModelStatusDisabled {
			panic(fmt.Errorf("model %s not found or disabled", payload.ModelID))
		}

		req, _, err := (chat.Request{
			Model:    mod.ModelId,
			Messages: payload.ContextMessages,
		}).Init().FixContextWindow(ct, 5, 1024*200, 2000)
		if err != nil {
			panic(fmt.Errorf("fix chat request failed: %w", err))
		}

		// 调用 AI 系统
		resp, err := ct.Chat(ctx, *req)
		if err != nil {
			panic(fmt.Errorf("chat failed: %w", err))
		}

		if resp.ErrorCode != "" {
			panic(fmt.Errorf("chat failed: %s %s", resp.ErrorCode, resp.Error))
		}

		inputTokens, _ := chat.MessageTokenCount(req.Messages, req.Model)
		outputTokens, _ := chat.MessageTokenCount(
			chat.Messages{{
				Role:    "assistant",
				Content: resp.Text,
			}}, req.Model,
		)

		tokenConsumed := int64(inputTokens + outputTokens)
		// 免费请求不计费
		leftCount, _ := svc.Chat.FreeChatRequestCounts(ctx, payload.UserID, req.Model)
		quotaConsumed := ternary.IfLazy(
			leftCount > 0,
			func() int64 { return 0 },
			func() int64 {
				return coins.GetTextModelCoins(mod.ToCoinModel(), int64(inputTokens), int64(outputTokens))
			},
		)

		// 更新消息状态
		msg := repo.ChatGroupMessageUpdate{
			Message:       resp.Text,
			TokenConsumed: tokenConsumed,
			QuotaConsumed: quotaConsumed,
			Status:        repo.MessageStatusSucceed,
		}
		if err := rep.ChatGroup.UpdateChatMessage(ctx, payload.GroupID, payload.UserID, payload.MessageID, msg); err != nil {
			panic(fmt.Errorf("update chat message failed: %w", err))
		}

		// 更新免费聊天次数
		if err := svc.Chat.UpdateFreeChatCount(ctx, payload.UserID, req.Model); err != nil {
			log.With(payload).Errorf("update free chat count failed: %s", err)
		}

		// 扣除智慧果
		if quotaConsumed > 0 {
			if err := rep.Quota.QuotaConsume(ctx, payload.UserID, quotaConsumed, repo.NewQuotaUsedMeta("group_chat", req.Model)); err != nil {
				log.Errorf("used quota add failed: %s", err)
			}
		}

		return rep.Queue.Update(
			context.TODO(),
			payload.GetID(),
			repo.QueueTaskStatusSuccess,
			EmptyResult{},
		)
	}
}
