package slacksub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/lestrrat/go-pdebug"
	"github.com/nlopes/slack"
	"golang.org/x/net/context"
	"google.golang.org/cloud/pubsub"
)

// This is the component that pulls messages from Cloud Pubsub

type Subscriber struct {
	Client          *pubsub.Client
	Done            chan struct{}
	HTTPClient      *http.Client
	MessageCallback func(*slack.MessageEvent) error
	Msgch           chan *pubsub.Message
	SlackgwURL      string
	AuthToken       string
	Topic           string
}

func New(cl *pubsub.Client, topic, slackgwURL, authtoken string) *Subscriber {
	return &Subscriber{
		Client:     cl,
		Done:       make(chan struct{}),
		HTTPClient: &http.Client{},
		Msgch:      make(chan *pubsub.Message),
		SlackgwURL: slackgwURL,
		AuthToken:  authtoken,
		Topic:      topic,
	}
}

func (sub *Subscriber) Close() {
	close(sub.Done)
}

func (sub *Subscriber) Run() {
	done := sub.Done
	sigCh := make(chan os.Signal, 16)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)

	go sub.keepFetching()
	go sub.keepProcessing()

	for {
		select {
		case <-done:
		case <-sigCh:
			return
		}
	}
}

func (sub *Subscriber) Reply(ch, s string) error {
	values := url.Values{
		"channel": []string{ch},
		"message": []string{s},
	}
	buf := bytes.Buffer{}
	buf.WriteString(values.Encode())

	req, err := http.NewRequest("POST", sub.SlackgwURL+"/post", &buf)
	if err != nil {
		return err
	}

	if authtoken := sub.AuthToken; authtoken != "" {
		req.Header.Set("X-Slackgw-Auth", authtoken)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_, err = sub.HTTPClient.Do(req)
	return err
}

func (sub *Subscriber) keepFetching() {
	if pdebug.Enabled {
		g := pdebug.Marker("b.keepFetching")
		defer g.End()
	}

	cl := sub.Client
	ch := sub.Msgch
	subscription := cl.Subscription(sub.Topic)
	backoff := 1000
	for loop := true; loop; {
		select {
		case <-sub.Done:
			loop = false
			continue
		default:
		}

		iter, err := subscription.Pull(context.Background())
		if err != nil {
			if pdebug.Enabled {
				pdebug.Printf("pull from '%s' failed: %s", subscription.Name(), err)
				pdebug.Printf("backing off for %d milliseconds", backoff)
			}
			// we need to backoff
			time.Sleep(time.Duration(backoff) * time.Millisecond)
			if backoff < 5*60*1000 {
				backoff = int(float64(backoff) * 1.2)
			}
			continue
		}

		backoff = 1000

		for {
			msg, err := iter.Next()
			if err != nil {
				if pdebug.Enabled {
					pdebug.Printf("iter.Next failed: %s", err)
				}
				break
			}
			if pdebug.Enabled {
				pdebug.Printf("New message arrived")
			}
			ch <- msg
		}
	}
}

type msgev struct {
	Type string     `json:"Type"`
	Data *slack.Msg `json:"Data"`
}

func (sub *Subscriber) keepProcessing() {
	if pdebug.Enabled {
		g := pdebug.Marker("b.keepProcessing")
		defer g.End()
	}

	done := sub.Done
	msgch := sub.Msgch
	for loop := true; loop; {
		var msg *pubsub.Message
		select {
		case <-done:
			loop = false
			continue
		case msg = <-msgch:
			// this needs to be in its own method because we want to call
			// defer msg.Done(true)
			if err := sub.processMessage(msg); err != nil {
				if pdebug.Enabled {
					pdebug.Printf("failed to process message: %s", err)
				}
			}
		}
	}
}

func (sub *Subscriber) processMessage(msg *pubsub.Message) error {
	defer msg.Done(true) // don't forget!
	if pdebug.Enabled {
		g := pdebug.Marker("b.processMessage")
		defer g.End()
	}

	var ev slack.MessageEvent
	in := msgev{Data: &ev.Msg}
	if err := json.Unmarshal(msg.Data, &in); err != nil {
		if pdebug.Enabled {
			pdebug.Printf("unmarshal failed: %s", err)
		}
		return err
	}

	if pdebug.Enabled {
		pdebug.Printf("incoming message '%s'", ev.Text)
	}

	cb := sub.MessageCallback
	if cb == nil {
		if pdebug.Enabled {
			pdebug.Printf("no message callback available, ignoring")
		}
		return nil
	}

	defer func() {
		if err := recover(); err != nil {
			debug.PrintStack()
			// Try to notify the client
			sub.Reply(
				ev.Channel,
				fmt.Sprintf("there was an error:\n```\n%s\n```\n", err),
			)
		}
	}()
	return cb(&ev)
}