package telegram

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/hako/durafmt"
	"github.com/nekomeowww/alertmanager-bot/pkg/alertmanager"
	"github.com/oklog/run"
	"github.com/pkg/errors"
	"github.com/prometheus/alertmanager/api/v2/models"
	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
	"gopkg.in/tucnak/telebot.v2"
)

const (
	CommandStart = "/start"
	CommandStop  = "/stop"
	CommandHelp  = "/help"
	CommandChats = "/chats"
	CommandID    = "/id"

	CommandStatus   = "/status"
	CommandAlerts   = "/alerts"
	CommandSilences = "/silences"

	responseAlertsNotConfigured = "该聊天当前还没有配置好来接收警报... 😕\n\n" +
		"要求 Alertmanager 的一个管理员添加一个 webhook，并把 `/webhooks/telegram/%d` 作为 URL."

	responseStartPrivate = "好的哦，%s 。这边将会向你推送最新的状态！\n" + CommandHelp
	responseStartGroup   = "好的哦，这边将会在群组推送最新的状态！\n" + CommandHelp
	responseStop         = "好的！这边之后都不会推送信息了。\n" + CommandHelp
	ResponseHelp         = `
我是一个用于 Telegram 的 Prometheus AlertManager Bot。将会通知你有关告警的信息。
你也可以向我询问 ` + CommandStatus + `, ` + CommandAlerts + ` & ` + CommandSilences + `

可用的命令: 
` + CommandStart + ` - 订阅告警
` + CommandStop + ` - 取消订阅告警
` + CommandStatus + ` - 打印当前状态
` + CommandAlerts + ` - 列出所有告警
` + CommandSilences + ` - 列出所有屏蔽告警
` + CommandChats + ` - 列出所有订阅告警的联系人和群组
` + CommandID + ` - 发送发送者的 Telegram ID（适用于所有 Telegram 用户）。
`
)

// BotChatStore is all the Bot needs to store and read.
type BotChatStore interface {
	List() ([]*telebot.Chat, error)
	Get(telebot.ChatID) (*telebot.Chat, error)
	Add(*telebot.Chat) error
	Remove(*telebot.Chat) error
}

// ChatNotFoundErr returned by the store if a chat isn't found.
var ChatNotFoundErr = errors.New("chat not found in store")

type Telebot interface {
	Start()
	Stop()
	Send(to telebot.Recipient, what interface{}, options ...interface{}) (*telebot.Message, error)
	Notify(to telebot.Recipient, action telebot.ChatAction) error
	Handle(endpoint interface{}, handler interface{})
}

type Alertmanager interface {
	ListAlerts(context.Context, string, bool) ([]*types.Alert, error)
	ListSilences(context.Context) ([]*types.Silence, error)
	Status(context.Context) (*models.AlertmanagerStatus, error)
}

// Bot runs the alertmanager telegram.
type Bot struct {
	addr         string
	admins       []int // must be kept sorted
	alertmanager Alertmanager
	templates    *template.Template
	chats        BotChatStore
	logger       log.Logger
	revision     string
	startTime    time.Time

	telegram Telebot

	commandEvents func(command string)
}

// BotOption passed to NewBot to change the default instance.
type BotOption func(b *Bot) error

// NewBot creates a Bot with the UserStore and telegram telegram.
func NewBot(chats BotChatStore, token string, admin int, opts ...BotOption) (*Bot, error) {
	poller := &telebot.LongPoller{
		Timeout: 10 * time.Second,
	}

	bot, err := telebot.NewBot(telebot.Settings{
		Token:  token,
		Poller: poller,
	})
	if err != nil {
		return nil, err
	}

	return NewBotWithTelegram(chats, bot, admin, opts...)
}

func NewBotWithTelegram(chats BotChatStore, bot Telebot, admin int, opts ...BotOption) (*Bot, error) {
	b := &Bot{
		logger:        log.NewNopLogger(),
		telegram:      bot,
		chats:         chats,
		addr:          "127.0.0.1:8080",
		admins:        []int{admin},
		commandEvents: func(command string) {},
	}

	for _, opt := range opts {
		if err := opt(b); err != nil {
			return nil, err
		}
	}

	return b, nil
}

// WithLogger sets the logger for the Bot as an option.
func WithLogger(l log.Logger) BotOption {
	return func(b *Bot) error {
		b.logger = l
		return nil
	}
}

// WithCommandEvent sets a func to call whenever commands are handled.
func WithCommandEvent(callback func(command string)) BotOption {
	return func(b *Bot) error {
		b.commandEvents = callback
		return nil
	}
}

// WithAddr sets the internal listening addr of the bot's web server receiving webhooks.
func WithAddr(addr string) BotOption {
	return func(b *Bot) error {
		b.addr = addr
		return nil
	}
}

func WithAlertmanager(alertmanager Alertmanager) BotOption {
	return func(b *Bot) error {
		b.alertmanager = alertmanager
		return nil
	}
}

// WithTemplates uses Alertmanager template to render messages for Telegram.
func WithTemplates(alertmanager *url.URL, templatePaths ...string) BotOption {
	return func(b *Bot) error {
		funcs := template.DefaultFuncs
		funcs["since"] = func(t time.Time) string {
			return durafmt.Parse(time.Since(t)).String()
		}
		funcs["duration"] = func(start time.Time, end time.Time) string {
			return durafmt.Parse(end.Sub(start)).String()
		}

		template.DefaultFuncs = funcs

		tmpl, err := template.FromGlobs(templatePaths...)
		if err != nil {
			return err
		}

		tmpl.ExternalURL = alertmanager
		b.templates = tmpl

		return nil
	}
}

// WithRevision is setting the Bot's revision for status commands.
func WithRevision(r string) BotOption {
	return func(b *Bot) error {
		b.revision = r
		return nil
	}
}

// WithStartTime is setting the Bot's start time for status commands.
func WithStartTime(st time.Time) BotOption {
	return func(b *Bot) error {
		b.startTime = st
		return nil
	}
}

// WithExtraAdmins allows the specified additional user IDs to issue admin
// commands to the bot.
func WithExtraAdmins(ids ...int) BotOption {
	return func(b *Bot) error {
		b.admins = append(b.admins, ids...)
		sort.Ints(b.admins)
		return nil
	}
}

// SendAdminMessage to the admin's ID with a message.
func (b *Bot) SendAdminMessage(adminID int, message string) {
	_, _ = b.telegram.Send(&telebot.User{ID: adminID}, message)
}

// isAdminID returns whether id is one of the configured admin IDs.
func (b *Bot) isAdminID(id int) bool {
	i := sort.SearchInts(b.admins, id)
	return i < len(b.admins) && b.admins[i] == id
}

// Run the telegram and listen to messages send to the telegram.
func (b *Bot) Run(ctx context.Context, webhooks <-chan alertmanager.TelegramWebhook) error {
	b.telegram.Handle(CommandStart, b.middleware(b.handleStart))
	b.telegram.Handle(CommandStop, b.middleware(b.handleStop))
	b.telegram.Handle(CommandHelp, b.middleware(b.handleHelp))
	b.telegram.Handle(CommandChats, b.middleware(b.handleChats))
	b.telegram.Handle(CommandID, b.middleware(b.handleID))
	b.telegram.Handle(CommandStatus, b.middleware(b.handleStatus))
	b.telegram.Handle(CommandAlerts, b.middleware(b.handleAlerts))
	b.telegram.Handle(CommandSilences, b.middleware(b.handleSilences))

	var gr run.Group
	{
		gr.Add(func() error {
			return b.sendWebhook(ctx, webhooks)
		}, func(err error) {
		})
	}
	{
		gr.Add(func() error {
			b.telegram.Start()
			return nil
		}, func(err error) {
			b.telegram.Stop()
		})
	}

	return gr.Run()
}

// sendWebhook sends messages received via webhook to all subscribed chats.
func (b *Bot) sendWebhook(ctx context.Context, webhooks <-chan alertmanager.TelegramWebhook) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case w := <-webhooks:
			chat, err := b.chats.Get(telebot.ChatID(w.ChatID))
			if err != nil {
				if errors.Is(err, ChatNotFoundErr) {
					level.Warn(b.logger).Log("msg", "chat is not subscribed for alerts", "chat_id", w.ChatID, "err", err)
					continue
				}
				return err
			}

			data := &template.Data{
				Receiver:          w.Message.Receiver,
				Status:            w.Message.Status,
				Alerts:            w.Message.Alerts,
				GroupLabels:       w.Message.GroupLabels,
				CommonLabels:      w.Message.CommonLabels,
				CommonAnnotations: w.Message.CommonAnnotations,
				ExternalURL:       w.Message.ExternalURL,
			}

			out, err := b.templates.ExecuteHTMLString(`{{ template "telegram.default" . }}`, data)
			if err != nil {
				level.Warn(b.logger).Log("msg", "failed to template alerts", "err", err)
				continue
			}

			_, err = b.telegram.Send(chat, b.truncateMessage(out), &telebot.SendOptions{ParseMode: telebot.ModeHTML})
			if err != nil {
				level.Warn(b.logger).Log("msg", "failed to send message with alerts", "err", err)
				continue
			}
		}
	}
}

func (b *Bot) middleware(next func(*telebot.Message) error) func(*telebot.Message) {
	return func(m *telebot.Message) {
		if m.IsService() {
			return
		}
		if !b.isAdminID(m.Sender.ID) && m.Text != CommandID {
			level.Info(b.logger).Log(
				"msg", "dropping message from forbidden sender",
				"sender_id", m.Sender.ID,
				"sender_username", m.Sender.Username,
			)
			return
		}

		command := strings.Split(m.Text, " ")[0]
		b.commandEvents(command)

		level.Debug(b.logger).Log("msg", "message received", "text", m.Text)
		if err := next(m); err != nil {
			level.Warn(b.logger).Log("msg", "failed to handle command", "err", err)
		}
	}
}

func (b *Bot) handleStart(message *telebot.Message) error {
	if err := b.chats.Add(message.Chat); err != nil {
		level.Warn(b.logger).Log("msg", "failed to add chat to chat store", "err", err)
		_, err = b.telegram.Send(message.Chat, "对不起，无法把该聊天加入订阅列表。")
		return err
	}

	level.Info(b.logger).Log(
		"msg", "user subscribed",
		"username", message.Sender.Username,
		"user_id", message.Sender.ID,
		"chat_id", message.Chat.ID,
	)

	switch message.Chat.Type {
	case telebot.ChatPrivate:
		_, err := b.telegram.Send(message.Chat, fmt.Sprintf(responseStartPrivate, message.Sender.FirstName))
		return err
	case telebot.ChatGroup:
	case telebot.ChatSuperGroup:
		_, err := b.telegram.Send(message.Chat, responseStartGroup)
		return err
	default:
		_, err := b.telegram.Send(message.Chat, "对不起，无法把该聊天加入订阅列表。")
		return err
	}

	return nil
}

func (b *Bot) handleStop(message *telebot.Message) error {
	if err := b.chats.Remove(message.Chat); err != nil {
		level.Warn(b.logger).Log("msg", "failed to remove chat from chat store", "err", err)
		_, err = b.telegram.Send(message.Chat, "对不起，无法把该聊天从订阅列表中移除")
		return err
	}

	_, err := b.telegram.Send(message.Chat, fmt.Sprintf(responseStop, message.Sender.FirstName))
	level.Info(b.logger).Log(
		"msg", "user unsubscribed",
		"username", message.Sender.Username,
		"user_id", message.Sender.ID,
	)
	return err
}

func (b *Bot) handleHelp(message *telebot.Message) error {
	_, err := b.telegram.Send(message.Chat, ResponseHelp)
	return err
}

func (b *Bot) handleChats(message *telebot.Message) error {
	chats, err := b.chats.List()
	if err != nil {
		level.Warn(b.logger).Log("msg", "failed to list chats from chat store", "err", err)
		_, err = b.telegram.Send(message.Chat, "对不起，没办法列出所有订阅列表")
		return err
	}

	if len(chats) == 0 {
		_, err = b.telegram.Send(message.Chat, "当前还没有人订阅过")
		return err
	}

	list := ""
	for _, chat := range chats {
		if chat.Type == telebot.ChatPrivate {
			list = list + fmt.Sprintf("@%s\n", chat.Username)
		} else if chat.Type == telebot.ChatGroup || chat.Type == telebot.ChatSuperGroup {
			fmt.Printf("%+v", chat)
			list = list + fmt.Sprintf("@%s\n", chat.Title)
		}
	}

	_, err = b.telegram.Send(message.Chat, "当前有以下聊天订阅了: \n"+list)
	return err
}

func (b *Bot) handleID(message *telebot.Message) error {
	if message.Private() {
		_, err := b.telegram.Send(message.Chat, fmt.Sprintf("你的 ID 是: %d", message.Sender.ID))
		return err
	}

	_, err := b.telegram.Send(message.Chat, fmt.Sprintf("你的 ID 是 %d\n聊天 ID 是 %d", message.Sender.ID, message.Chat.ID))
	return err
}

func (b *Bot) handleStatus(message *telebot.Message) error {
	status, err := b.alertmanager.Status(context.TODO())
	if err != nil {
		level.Warn(b.logger).Log("msg", "failed to get status", "err", err)
		_, err = b.telegram.Send(message.Chat, fmt.Sprintf("获取状态失败... %v", err))
		return err
	}

	uptime := durafmt.Parse(time.Since(time.Time(*status.Uptime)))
	uptimeBot := durafmt.Parse(time.Since(b.startTime))

	_, err = b.telegram.Send(
		message.Chat,
		fmt.Sprintf(
			"*AlertManager*\n版本: %s\n运行时间: %s\n*AlertManager Bot*\n版本: %s\n运行时间: %s",
			*status.VersionInfo.Version,
			uptime,
			b.revision,
			uptimeBot,
		),
		&telebot.SendOptions{ParseMode: telebot.ModeMarkdown},
	)
	return err
}

func (b *Bot) handleAlerts(message *telebot.Message) error {
	status, err := b.alertmanager.Status(context.TODO())
	if err != nil {
		level.Warn(b.logger).Log("msg", "failed to get status with config", "err", err)
		_, err = b.telegram.Send(message.Chat, fmt.Sprintf("列出告警失败... %v", err))
		return err
	}

	receiver, err := receiverFromConfig(*status.Config.Original, message.Chat.ID)
	if err != nil || receiver == "" {
		_, err := b.telegram.Send(message.Chat, fmt.Sprintf(responseAlertsNotConfigured, message.Chat.ID), &telebot.SendOptions{ParseMode: telebot.ModeMarkdown})
		return err
	}

	silenced := false
	if strings.Contains(message.Payload, "silenced") {
		silenced = true
	}

	alerts, err := b.alertmanager.ListAlerts(context.TODO(), receiver, silenced)
	if err != nil {
		level.Warn(b.logger).Log("msg", "failed to list alerts", "err", err)
		_, err = b.telegram.Send(message.Chat, fmt.Sprintf("列出告警失败... %v", err))
		return err
	}

	if len(alerts) == 0 {
		_, err = b.telegram.Send(message.Chat, "当前没有告警！ 🎉")
		return err
	}

	out, err := b.tmplAlerts(alerts...)
	if err != nil {
		level.Warn(b.logger).Log("msg", "failed to template alerts", "err", err)
		return nil
	}

	_, err = b.telegram.Send(message.Chat, b.truncateMessage(out), &telebot.SendOptions{
		ParseMode: telebot.ModeHTML,
	})
	return err
}

func receiverFromConfig(c string, id int64) (string, error) {
	if c == "" {
		return "", fmt.Errorf("config is empty")
	}

	config, err := config.Load(c)
	if err != nil {
		return "", err
	}

	for _, receiver := range config.Receivers {
		for _, webhook := range receiver.WebhookConfigs {
			path := webhook.URL.Path
			if strings.HasPrefix(path, "/webhooks/telegram/") {
				parseID, err := strconv.ParseInt(strings.TrimPrefix(path, "/webhooks/telegram/"), 10, 64)
				if err != nil {
					return "", err
				}
				if parseID == id {
					return receiver.Name, nil
				}
			}
		}
	}

	return "", nil
}

func (b *Bot) handleSilences(message *telebot.Message) error {
	silences, err := b.alertmanager.ListSilences(context.TODO())
	if err != nil {
		_, err = b.telegram.Send(message.Chat, fmt.Sprintf("列出屏蔽告警失败... %v", err))
		return err
	}

	if len(silences) == 0 {
		_, err = b.telegram.Send(message.Chat, "当前没有已屏蔽的告警")
		return err
	}

	var out string
	for _, silence := range silences {
		out = out + alertmanager.SilenceMessage(silence) + "\n"
	}

	_, err = b.telegram.Send(message.Chat, out, &telebot.SendOptions{ParseMode: telebot.ModeMarkdown})
	return err
}

func (b *Bot) tmplAlerts(alerts ...*types.Alert) (string, error) {
	data := b.templates.Data("default", nil, alerts...)

	out, err := b.templates.ExecuteHTMLString(`{{ template "telegram.default" . }}`, data)
	if err != nil {
		return "", err
	}

	return out, nil
}

// Truncate very big message.
func (b *Bot) truncateMessage(str string) string {
	truncateMsg := str
	if len(str) > 4095 { // telegram API can only support 4096 bytes per message
		level.Warn(b.logger).Log("msg", "Message is bigger than 4095, truncate...")
		// find the end of last alert, we do not want break the html tags
		i := strings.LastIndex(str[0:4080], "\n\n") // 4080 + "\n<b>[SNIP]</b>" == 4095
		if i > 1 {
			truncateMsg = str[0:i] + "\n<b>[SNIP]</b>"
		} else {
			truncateMsg = "Message is too long... can't send.."
			level.Warn(b.logger).Log("msg", "truncateMessage: Unable to find the end of last alert.")
		}
		return truncateMsg
	}
	return truncateMsg
}
