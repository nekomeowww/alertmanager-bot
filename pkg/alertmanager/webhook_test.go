package alertmanager

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-kit/kit/log"
	"github.com/prometheus/alertmanager/notify/webhook"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
)

const validWebhook = `{"receiver":"telegram","status":"firing","alerts":[{"status":"firing","labels":{"alertname":"Fire","severity":"critical"},"annotations":{"message":"Something is on fire"},"startsAt":"2018-11-04T22:43:58.283995108+01:00","endsAt":"2018-11-04T22:46:58.283995108+01:00","generatorURL":"http://localhost:9090/graph?g0.expr=vector%28666%29\u0026g0.tab=1"}],"groupLabels":{"alertname":"Fire"},"commonLabels":{"alertname":"Fire","severity":"critical"},"commonAnnotations":{"message":"Something is on fire"},"externalURL":"http://localhost:9093","version":"4","groupKey":"{}:{alertname=\"Fire\"}"}`

func TestHandleWebhook(t *testing.T) {
	webhooks := make(chan webhook.Message, 1)

	type checkFunc func(resp *http.Response) error

	checkStatusCode := func(code int) checkFunc {
		return func(resp *http.Response) error {
			if resp.StatusCode != code {
				return fmt.Errorf("statusCode %d expected, got %d", code, resp.StatusCode)
			}
			return nil
		}
	}

	testcases := []struct {
		name   string
		req    func() *http.Request
		checks []checkFunc
	}{
		{
			name: "NotPOST",
			req: func() *http.Request {
				req, _ := http.NewRequest(http.MethodGet, "/", nil)
				return req
			},
			checks: []checkFunc{
				checkStatusCode(http.StatusMethodNotAllowed),
			},
		},
		{
			name: "EmptyBody",
			req: func() *http.Request {
				var body io.Reader
				req, _ := http.NewRequest(http.MethodPost, "/", body)
				return req
			},
			checks: []checkFunc{
				checkStatusCode(http.StatusBadRequest),
			},
		},
		{
			name: "InvalidJSON",
			req: func() *http.Request {
				body := bytes.NewBufferString(`[]`)
				req, _ := http.NewRequest(http.MethodPost, "/", body)
				return req
			},
			checks: []checkFunc{
				checkStatusCode(http.StatusBadRequest),
			},
		},
		{
			name: "ValidWebhook",
			req: func() *http.Request {
				body := bytes.NewBufferString(validWebhook)
				req, _ := http.NewRequest(http.MethodPost, "/", body)
				return req
			},
			checks: []checkFunc{
				checkStatusCode(http.StatusOK),

				func(resp *http.Response) error {
					var expected webhook.Message
					if err := json.Unmarshal([]byte(validWebhook), &expected); err != nil {
						return err
					}

					webhook := <-webhooks
					assert.Equal(t, expected, webhook)
					return nil
				},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			counter := prometheus.NewCounter(prometheus.CounterOpts{
				Name: "alertmanagerbot_webhooks_total",
				Help: "Number of webhooks received by this bot",
			})

			handler := HandleWebhook(log.NewNopLogger(), counter, webhooks)

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, tc.req())

			for _, check := range tc.checks {
				if err := check(rec.Result()); err != nil {
					t.Error(err)
				}
			}
		})
	}
}