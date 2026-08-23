// Package notify сообщает о сбоях.
//
// Самый частый способ остаться без бэкапов - не заметить, что их давно
// нет. Поэтому здесь два разных механизма:
//
//   - уведомление об ошибке: пришло письмо - что-то сломалось;
//   - сигнал «я жив»: после каждого успешного бэкапа дёргается внешний
//     адрес, и если дёргать перестали, тревогу поднимает уже внешняя
//     служба. Только так ловится случай «компьютер выключен, задание
//     не запускалось, ошибки не было - потому что не было ничего».
//
// По умолчанию об удачных бэкапах не сообщается. Ежедневное «всё хорошо»
// перестают читать через неделю, и вместе с ним перестают замечать
// единственное «всё плохо».
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

type Level string

const (
	LevelError   Level = "error"
	LevelWarning Level = "warning"
	LevelInfo    Level = "info"
)

type Message struct {
	Level  Level
	Server string
	Title  string
	Body   string
}

func (m Message) mark() string {
	switch m.Level {
	case LevelError:
		return "СБОЙ"
	case LevelWarning:
		return "ВНИМАНИЕ"
	}
	return "OK"
}

func (m Message) subject() string {
	if m.Server != "" {
		return fmt.Sprintf("[%s] %s: %s", m.mark(), m.Server, m.Title)
	}
	return fmt.Sprintf("[%s] %s", m.mark(), m.Title)
}

func (m Message) plain() string {
	if m.Body == "" {
		return m.subject()
	}
	return m.subject() + "\n\n" + m.Body
}

type Config struct {
	Enabled bool `json:"enabled"`

	// OnSuccess по умолчанию выключено намеренно - см. комментарий к пакету.
	OnSuccess bool `json:"on_success,omitempty"`
	// OnPartial: снимок сделан, но часть модулей не отработала. Это
	// отдельное состояние, и молчать о нём нельзя: такой бэкап выглядит
	// целым в списке, а при восстановлении окажется без баз.
	OnPartial bool `json:"on_partial"`

	Telegram TelegramConfig `json:"telegram,omitempty"`
	Webhook  WebhookConfig  `json:"webhook,omitempty"`
	Mail     MailConfig     `json:"mail,omitempty"`

	// HeartbeatURL дёргается после каждого успешного бэкапа.
	// Подходит любой сервис мёртвой руки: healthchecks.io, Uptime Kuma,
	// собственный скрипт.
	HeartbeatURL string `json:"heartbeat_url,omitempty"`

	// StaleAfter - через сколько без успешного бэкапа считать сервер
	// заброшенным и сообщить об этом.
	StaleAfter Duration `json:"stale_after,omitempty"`
}

type TelegramConfig struct {
	ChatID string `json:"chat_id,omitempty"`
	// Token лежит не здесь, а в защищённом хранилище десктопа.
	Token string `json:"-"`
}

type WebhookConfig struct {
	// URL совместим со Slack, Mattermost и Discord: все они принимают
	// JSON с полем text.
	URL string `json:"url,omitempty"`
}

type MailConfig struct {
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	User string `json:"user,omitempty"`
	Pass string `json:"-"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// Duration - время в человеческом виде: "48h" вместо наносекунд.
type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("autobak: некорректный срок %q: %w", b, err)
	}
	*d = Duration(v)
	return nil
}

func DefaultConfig() Config {
	return Config{
		OnPartial:  true,
		StaleAfter: Duration(36 * time.Hour),
	}
}

// Configured сообщает, есть ли хоть один настроенный канал.
func (c Config) Configured() bool {
	return c.Telegram.ChatID != "" || c.Webhook.URL != "" || c.Mail.Host != ""
}

// ShouldSend решает, стоит ли беспокоить человека этим сообщением.
func (c Config) ShouldSend(m Message) bool {
	if !c.Enabled {
		return false
	}
	switch m.Level {
	case LevelError:
		return true
	case LevelWarning:
		return c.OnPartial
	}
	return c.OnSuccess
}

var client = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	},
}

// Send отправляет сообщение во все настроенные каналы.
//
// Ошибка одного канала не отменяет остальные: смысл уведомления в том,
// чтобы дойти хоть как-нибудь. Возвращается только сводная ошибка, если
// не сработал ни один канал.
func Send(ctx context.Context, c Config, m Message) error {
	if !c.ShouldSend(m) {
		return nil
	}
	var attempted, failed []string

	if c.Telegram.ChatID != "" && c.Telegram.Token != "" {
		attempted = append(attempted, "telegram")
		if err := sendTelegram(ctx, c.Telegram, m); err != nil {
			failed = append(failed, "telegram: "+err.Error())
		}
	}
	if c.Webhook.URL != "" {
		attempted = append(attempted, "webhook")
		if err := sendWebhook(ctx, c.Webhook.URL, m); err != nil {
			failed = append(failed, "webhook: "+err.Error())
		}
	}
	if c.Mail.Host != "" && c.Mail.To != "" {
		attempted = append(attempted, "почта")
		if err := sendMail(c.Mail, m); err != nil {
			failed = append(failed, "почта: "+err.Error())
		}
	}

	if len(attempted) == 0 {
		return nil
	}
	if len(failed) == len(attempted) {
		return fmt.Errorf("autobak: уведомление не доставлено: %s", strings.Join(failed, "; "))
	}
	return nil
}

func sendTelegram(ctx context.Context, tg TelegramConfig, m Message) error {
	body, err := json.Marshal(map[string]any{
		"chat_id": tg.ChatID,
		"text":    m.plain(),
		// Разметка не используется намеренно: в сообщение попадают пути и
		// тексты ошибок, и любой символ вроде _ сломал бы разбор, а
		// Telegram отверг бы сообщение целиком.
		"disable_web_page_preview": true,
	})
	if err != nil {
		return err
	}
	url := "https://api.telegram.org/bot" + tg.Token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return redact(err.Error(), tg.Token)
	}
	req.Header.Set("Content-Type", "application/json")
	return doAndCheck(req, tg.Token)
}

func sendWebhook(ctx context.Context, url string, m Message) error {
	body, err := json.Marshal(map[string]any{
		"text":     m.plain(),
		"username": "autobak",
		"level":    string(m.Level),
		"server":   m.Server,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return redact(err.Error(), url)
	}
	req.Header.Set("Content-Type", "application/json")
	// URL webhook сам по себе секрет: у Slack/Discord/Mattermost токен
	// зашит в адрес. Отдаём его в redact, чтобы при ошибке он не утёк в
	// журнал и в интерфейс.
	return doAndCheck(req, url)
}

// doAndCheck выполняет запрос и вычищает секрет из текста ошибки:
// адрес Telegram содержит токен бота, а ошибки попадают в журнал.
func doAndCheck(req *http.Request, secret string) error {
	resp, err := client.Do(req)
	if err != nil {
		return redact(err.Error(), secret)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf := make([]byte, 400)
		n, _ := resp.Body.Read(buf)
		return redact(fmt.Sprintf("%s: %s", resp.Status, strings.TrimSpace(string(buf[:n]))), secret)
	}
	return nil
}

type redactedError string

func (e redactedError) Error() string { return string(e) }

// redact убирает секрет из текста ошибки.
//
// Вычищаются обе формы: как есть и в процентном экранировании. Go
// подставляет в url.Error адрес запроса целиком, а адрес API Telegram
// содержит токен бота - то есть полный доступ к нему. В экранированном
// виде токен ищется потому, что нелатинские символы в адресе Go
// перекодирует, и поиск по литералу их не нашёл бы.
func redact(s, secret string) error {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "***")
		if esc := url.PathEscape(secret); esc != secret {
			s = strings.ReplaceAll(s, esc, "***")
		}
		if esc := url.QueryEscape(secret); esc != secret {
			s = strings.ReplaceAll(s, esc, "***")
		}
	}
	return redactedError(s)
}

func sendMail(c MailConfig, m Message) error {
	if c.Port == 0 {
		c.Port = 587
	}
	from := c.From
	if from == "" {
		from = c.User
	}
	to := strings.Split(c.To, ",")
	for i := range to {
		to[i] = strings.TrimSpace(to[i])
	}

	// Заголовки собираются вручную: тема с кириллицей должна уехать в
	// base64 по RFC 2047, иначе почтовые клиенты покажут кракозябры.
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", encodeHeader(m.subject()))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(m.plain())

	addr := fmt.Sprintf("%s:%d", c.Host, c.Port)
	var auth smtp.Auth
	if c.User != "" {
		auth = smtp.PlainAuth("", c.User, c.Pass, c.Host)
	}
	if err := smtp.SendMail(addr, auth, from, to, []byte(b.String())); err != nil {
		return redact(err.Error(), c.Pass)
	}
	return nil
}

func encodeHeader(s string) string {
	for i := range len(s) {
		if s[i] > 127 {
			return mimeEncode(s)
		}
	}
	return s
}

func mimeEncode(s string) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	src := []byte(s)
	var out strings.Builder
	for i := 0; i < len(src); i += 3 {
		var n uint32
		rem := len(src) - i
		n = uint32(src[i]) << 16
		if rem > 1 {
			n |= uint32(src[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(src[i+2])
		}
		out.WriteByte(chars[(n>>18)&63])
		out.WriteByte(chars[(n>>12)&63])
		if rem > 1 {
			out.WriteByte(chars[(n>>6)&63])
		} else {
			out.WriteByte('=')
		}
		if rem > 2 {
			out.WriteByte(chars[n&63])
		} else {
			out.WriteByte('=')
		}
	}
	return "=?UTF-8?B?" + out.String() + "?="
}

// Heartbeat сообщает внешней службе, что бэкап прошёл.
//
// Это единственное, что ловит самый неприятный сбой: программа вообще не
// запускалась. Уведомление об ошибке тут бессильно - ошибки не было,
// не было ничего. Внешняя служба замечает пропажу сигнала и поднимает
// тревогу сама.
func Heartbeat(ctx context.Context, url string) error {
	if url == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return redact(err.Error(), url)
	}
	// Ping-URL (healthchecks.io и подобные) содержит секретный идентификатор
	// - редактируем его из возможной ошибки.
	return doAndCheck(req, url)
}

// Stale строит сообщение о сервере, который давно не бэкапился.
func Stale(server string, last time.Time, after time.Duration) Message {
	if last.IsZero() {
		return Message{
			Level: LevelError, Server: server,
			Title: "бэкапов не было ни разу",
			Body:  "Сервер добавлен, но ни один бэкап так и не выполнился.",
		}
	}
	return Message{
		Level: LevelError, Server: server,
		Title: fmt.Sprintf("бэкапа нет уже %s", roundDur(time.Since(last))),
		Body: fmt.Sprintf(
			"Последний успешный бэкап: %s.\nПорог тревоги: %s.\n\n"+
				"Проверьте, выполняется ли задание в планировщике и доступен ли сервер.",
			last.Local().Format("02.01.2006 15:04"), roundDur(after)),
	}
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d ч", int(d.Hours()))
	}
	return fmt.Sprintf("%d дн", int(d.Hours()/24))
}
