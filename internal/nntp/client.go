// internal/nntp/client.go
// Stahuje články z Usenet skupin přes NNTP protokol.
// Podporuje paralelní stahování, retry logiku a ukládání do SQLite.
package nntp

import (
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Article reprezentuje jeden Usenet článek.
type Article struct {
	MessageID string
	Subject   string
	From      string
	Date      time.Time
	Newsgroup string
	Body      string
	References []string
}

// Config konfigurace NNTP klienta.
type Config struct {
	Server   string
	Port     int
	UseTLS   bool
	Username string
	Password string
	Timeout  time.Duration
}

// Client NNTP klient.
type Client struct {
	cfg  Config
	conn *textproto.Conn
	raw  net.Conn
}

// NewClient vytvoří nový NNTP klient (nepřipojí se).
func NewClient(cfg Config) *Client {
	if cfg.Port == 0 {
		if cfg.UseTLS {
			cfg.Port = 563
		} else {
			cfg.Port = 119
		}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &Client{cfg: cfg}
}

// Connect připojí se k NNTP serveru.
func (c *Client) Connect() error {
	addr := fmt.Sprintf("%s:%d", c.cfg.Server, c.cfg.Port)

	var (
		raw net.Conn
		err error
	)

	if c.cfg.UseTLS {
		raw, err = tls.DialWithDialer(
			&net.Dialer{Timeout: c.cfg.Timeout},
			"tcp", addr,
			&tls.Config{ServerName: c.cfg.Server},
		)
	} else {
		raw, err = net.DialTimeout("tcp", addr, c.cfg.Timeout)
	}
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}

	c.raw = raw
	c.conn = textproto.NewConn(raw)

	// Přečti uvítací banner (200 nebo 201)
	_, _, err = c.conn.ReadCodeLine(0)
	if err != nil {
		return fmt.Errorf("banner: %w", err)
	}

	// Přihlásit se pokud jsou credentials
	if c.cfg.Username != "" {
		if err := c.auth(); err != nil {
			return err
		}
	}

	slog.Info("NNTP connected", "server", addr)
	return nil
}

func (c *Client) auth() error {
	id, err := c.conn.Cmd("AUTHINFO USER %s", c.cfg.Username)
	if err != nil {
		return err
	}
	c.conn.StartResponse(id)
	code, _, err := c.conn.ReadCodeLine(0)
	c.conn.EndResponse(id)
	if err != nil && code != 381 {
		return fmt.Errorf("auth user: %w", err)
	}

	id, err = c.conn.Cmd("AUTHINFO PASS %s", c.cfg.Password)
	if err != nil {
		return err
	}
	c.conn.StartResponse(id)
	_, _, err = c.conn.ReadCodeLine(281)
	c.conn.EndResponse(id)
	if err != nil {
		return fmt.Errorf("auth pass: %w", err)
	}
	return nil
}

// Close zavře spojení.
func (c *Client) Close() error {
	if c.conn != nil {
		c.conn.Cmd("QUIT") //nolint
		return c.raw.Close()
	}
	return nil
}

// GroupInfo vrací info o newsgroup (první, poslední číslo článku, počet).
type GroupInfo struct {
	Name  string
	Count int64
	First int64
	Last  int64
}

// GetGroup přepne do skupiny a vrátí info.
func (c *Client) GetGroup(name string) (*GroupInfo, error) {
	id, err := c.conn.Cmd("GROUP %s", name)
	if err != nil {
		return nil, err
	}
	c.conn.StartResponse(id)
	defer c.conn.EndResponse(id)

	_, msg, err := c.conn.ReadCodeLine(211)
	if err != nil {
		return nil, fmt.Errorf("group %s: %w", name, err)
	}

	// "211 count first last groupname"
	parts := strings.Fields(msg)
	if len(parts) < 3 {
		return nil, fmt.Errorf("unexpected GROUP response: %s", msg)
	}

	count, _ := strconv.ParseInt(parts[0], 10, 64)
	first, _ := strconv.ParseInt(parts[1], 10, 64)
	last, _ := strconv.ParseInt(parts[2], 10, 64)

	return &GroupInfo{Name: name, Count: count, First: first, Last: last}, nil
}

// FetchArticle stáhne jeden článek podle čísla.
func (c *Client) FetchArticle(num int64) (*Article, error) {
	id, err := c.conn.Cmd("ARTICLE %d", num)
	if err != nil {
		return nil, err
	}
	c.conn.StartResponse(id)
	defer c.conn.EndResponse(id)

	_, _, err = c.conn.ReadCodeLine(220)
	if err != nil {
		// 423 = no article, přeskočit
		return nil, fmt.Errorf("article %d: %w", num, err)
	}

	raw, err := c.conn.ReadDotBytes()
	if err != nil {
		return nil, fmt.Errorf("read article %d: %w", num, err)
	}

	return parseArticle(string(raw))
}

// parseArticle parsuje raw NNTP článek do struktury Article.
func parseArticle(raw string) (*Article, error) {
	headerEnd := strings.Index(raw, "\n\n")
	if headerEnd == -1 {
		headerEnd = strings.Index(raw, "\r\n\r\n")
	}

	var headerPart, body string
	if headerEnd != -1 {
		headerPart = raw[:headerEnd]
		body = strings.TrimSpace(raw[headerEnd:])
	} else {
		headerPart = raw
	}

	a := &Article{Body: body}

	for _, line := range strings.Split(headerPart, "\n") {
		line = strings.TrimRight(line, "\r")
		colon := strings.Index(line, ": ")
		if colon == -1 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:colon]))
		val := strings.TrimSpace(line[colon+2:])

		switch key {
		case "message-id":
			a.MessageID = strings.Trim(val, "<>")
		case "subject":
			a.Subject = val
		case "from":
			a.From = val
		case "date":
			if t, err := parseDate(val); err == nil {
				a.Date = t
			}
		case "newsgroups":
			// Vzít první skupinu
			a.Newsgroup = strings.Fields(strings.Split(val, ",")[0])[0]
		case "references":
			for _, ref := range strings.Fields(val) {
				a.References = append(a.References, strings.Trim(ref, "<>"))
			}
		}
	}

	return a, nil
}

func parseDate(s string) (time.Time, error) {
	formats := []string{
		time.RFC1123Z,
		time.RFC1123,
		"2 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 -0700",
		"2 Jan 2006 15:04:05 MST",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse date: %s", s)
}

// FetchRange stáhne rozsah článků a posílá je do kanálu.
// Používá workers paralelních goroutin.
func (c *Client) FetchRange(group string, first, last int64, workers int) (<-chan *Article, <-chan error) {
	articles := make(chan *Article, workers*2)
	errs := make(chan error, 1)

	go func() {
		defer close(articles)
		defer close(errs)

		nums := make(chan int64, workers*4)

		// Producent čísel článků
		go func() {
			defer close(nums)
			for n := first; n <= last; n++ {
				nums <- n
			}
		}()

		// Pool workerů — každý má vlastní spojení (NNTP je stavový protokol)
		type result struct {
			a   *Article
			err error
		}
		results := make(chan result, workers*2)

		for i := 0; i < workers; i++ {
			go func() {
				// Každý worker si vytvoří vlastní klientské spojení
				wc := NewClient(c.cfg)
				if err := wc.Connect(); err != nil {
					results <- result{err: err}
					return
				}
				defer wc.Close()

				if _, err := wc.GetGroup(group); err != nil {
					results <- result{err: err}
					return
				}

				for num := range nums {
					a, err := wc.FetchArticle(num)
					if err != nil {
						// Loguj ale nepřeruš — 423 (no article) je normální
						slog.Debug("skip article", "num", num, "err", err)
						results <- result{a: nil}
						continue
					}
					a.Newsgroup = group
					results <- result{a: a}
				}
			}()
		}

		// Sbírej výsledky
		total := int(last - first + 1)
		for i := 0; i < total; i++ {
			r := <-results
			if r.err != nil {
				select {
				case errs <- r.err:
				default:
				}
				return
			}
			if r.a != nil {
				articles <- r.a
			}
		}
	}()

	return articles, errs
}

// Dummy pro případ že se někdo importuje jen kvůli interface
var _ io.Closer = (*Client)(nil)
