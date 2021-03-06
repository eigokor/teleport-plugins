package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/gravitational/teleport-plugins/access/integration"
	"github.com/gravitational/teleport/lib/auth/testauthority"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/trace"
	"github.com/nlopes/slack"
	"github.com/nlopes/slack/slacktest"

	. "gopkg.in/check.v1"
)

const (
	Host        = "localhost"
	HostID      = "00000000-0000-0000-0000-000000000000"
	Site        = "local-site"
	SlackSecret = "f9e77a2814566fe23d33dee5b853955b"
)

type SlackSuite struct {
	ctx         context.Context
	cancel      context.CancelFunc
	appConfig   Config
	app         *App
	publicURL   string
	me          *user.User
	slackServer *slacktest.Server
	teleport    *integration.TeleInstance
	tmpFiles    []*os.File
}

var _ = Suite(&SlackSuite{})

func TestSlackbot(t *testing.T) { TestingT(t) }

func (s *SlackSuite) SetUpSuite(c *C) {
	var err error
	log.SetLevel(log.DebugLevel)
	priv, pub, err := testauthority.New().GenerateKeyPair("")
	c.Assert(err, IsNil)
	t := integration.NewInstance(integration.InstanceConfig{ClusterName: Site, HostID: HostID, NodeName: Host, Priv: priv, Pub: pub})

	s.me, err = user.Current()
	c.Assert(err, IsNil)
	userRole, err := services.NewRole("foo", services.RoleSpecV3{
		Allow: services.RoleConditions{
			Logins:  []string{s.me.Username}, // cannot be empty
			Request: &services.AccessRequestConditions{Roles: []string{"admin"}},
		},
	})
	c.Assert(err, IsNil)
	t.AddUserWithRole(s.me.Username, userRole)

	accessPluginRole, err := services.NewRole("access-plugin", services.RoleSpecV3{
		Allow: services.RoleConditions{
			Logins: []string{"access-plugin"}, // cannot be empty
			Rules: []services.Rule{
				services.NewRule("access_request", []string{"list", "read", "update"}),
			},
		},
	})
	c.Assert(err, IsNil)
	t.AddUserWithRole("plugin", accessPluginRole)

	err = t.Create(nil, nil)
	c.Assert(err, IsNil)
	if err := t.Start(); err != nil {
		c.Fatalf("Unexpected response from Start: %v", err)
	}
	s.teleport = t
}

func (s *SlackSuite) SetUpTest(c *C) {
	s.ctx, s.cancel = context.WithTimeout(context.Background(), time.Second)
	s.publicURL = ""
	s.startSlack(c)

	auth := s.teleport.Process.GetAuthServer()
	certAuthorities, err := auth.GetCertAuthorities(services.HostCA, false)
	c.Assert(err, IsNil)
	pluginKey := s.teleport.Secrets.Users["plugin"].Key

	keyFile := s.newTmpFile(c, "auth.*.key")
	_, err = keyFile.Write(pluginKey.Priv)
	c.Assert(err, IsNil)
	keyFile.Close()

	certFile := s.newTmpFile(c, "auth.*.crt")
	_, err = certFile.Write(pluginKey.TLSCert)
	c.Assert(err, IsNil)
	certFile.Close()

	casFile := s.newTmpFile(c, "auth.*.cas")
	for _, ca := range certAuthorities {
		for _, keyPair := range ca.GetTLSKeyPairs() {
			_, err = casFile.Write(keyPair.Cert)
			c.Assert(err, IsNil)
		}
	}
	casFile.Close()

	authAddr, err := s.teleport.Process.AuthSSHAddr()
	c.Assert(err, IsNil)

	var conf Config
	conf.Teleport.AuthServer = authAddr.Addr
	conf.Teleport.ClientCrt = certFile.Name()
	conf.Teleport.ClientKey = keyFile.Name()
	conf.Teleport.RootCAs = casFile.Name()
	conf.Slack.Secret = SlackSecret
	conf.Slack.Token = "000000"
	conf.Slack.Channel = "test"
	conf.Slack.APIURL = "http://" + s.slackServer.ServerAddr + "/"
	conf.HTTP.ListenAddr = ":0"
	conf.HTTP.Insecure = true

	s.appConfig = conf
}

func (s *SlackSuite) TearDownTest(c *C) {
	s.shutdownApp(c)
	s.slackServer.Stop()
	s.cancel()
	for _, tmp := range s.tmpFiles {
		err := os.Remove(tmp.Name())
		c.Assert(err, IsNil)
	}
	s.tmpFiles = []*os.File{}
}

func (s *SlackSuite) newTmpFile(c *C, pattern string) (file *os.File) {
	file, err := ioutil.TempFile("", pattern)
	c.Assert(err, IsNil)
	s.tmpFiles = append(s.tmpFiles, file)
	return
}

func (s *SlackSuite) startSlack(c *C) {
	s.slackServer = slacktest.NewTestServer()
	s.slackServer.SetBotName("access_bot")
	go s.slackServer.Start()
}

func (s *SlackSuite) startApp(c *C) {
	var err error

	if s.publicURL != "" {
		s.appConfig.HTTP.PublicAddr = s.publicURL
	}
	s.app, err = NewApp(s.appConfig)
	c.Assert(err, IsNil)

	go func() {
		err = s.app.Run(s.ctx)
		c.Assert(err, IsNil)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*250)
	defer cancel()
	ok, err := s.app.WaitReady(ctx)
	c.Assert(err, IsNil)
	c.Assert(ok, Equals, true)
	if s.publicURL == "" {
		s.publicURL = s.app.PublicURL().String()
	}
}

func (s *SlackSuite) shutdownApp(c *C) {
	err := s.app.Shutdown(s.ctx)
	c.Assert(err, IsNil)
}

func (s *SlackSuite) createAccessRequest(c *C) services.AccessRequest {
	req, err := s.teleport.CreateAccessRequest(s.ctx, s.me.Username, "admin")
	c.Assert(err, IsNil)
	return req
}

func (s *SlackSuite) createExpiredAccessRequest(c *C) services.AccessRequest {
	req, err := s.teleport.CreateExpiredAccessRequest(s.ctx, s.me.Username, "admin")
	c.Assert(err, IsNil)
	return req
}

func (s *SlackSuite) checkPluginData(c *C, reqID string) PluginData {
	rawData, err := s.teleport.PollAccessRequestPluginData(s.ctx, "slack", reqID)
	c.Assert(err, IsNil)
	return DecodePluginData(rawData)
}

func (s *SlackSuite) postCallbackAndCheck(c *C, actionID, reqID string, expectedStatus int) {
	resp, err := s.postCallback(s.ctx, actionID, reqID)
	c.Assert(err, IsNil)
	c.Assert(resp.Body.Close(), IsNil)
	c.Assert(resp.StatusCode, Equals, expectedStatus)
}

func (s *SlackSuite) postCallback(ctx context.Context, actionID, reqID string) (*http.Response, error) {
	cb := &slack.InteractionCallback{
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{
					ActionID: actionID,
					Value:    reqID,
				},
			},
		},
	}

	payload, err := json.Marshal(cb)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	data := url.Values{
		"payload": {string(payload)},
	}
	body := data.Encode()

	stimestamp := fmt.Sprintf("%d", time.Now().Unix())
	hash := hmac.New(sha256.New, []byte(SlackSecret))
	_, err = hash.Write([]byte(fmt.Sprintf("v0:%s:%s", stimestamp, body)))
	if err != nil {
		return nil, trace.Wrap(err)
	}

	signature := hash.Sum(nil)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.publicURL, strings.NewReader(body))
	if err != nil {
		return nil, trace.Wrap(err)
	}
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("X-Slack-Request-Timestamp", stimestamp)
	req.Header.Add("X-Slack-Signature", "v0="+hex.EncodeToString(signature))

	response, err := http.DefaultClient.Do(req)
	return response, trace.Wrap(err)
}

// fetchSlackMessage and all the tests using it heavily rely on changes in slacktest package, see 13c57c4 commit.
func (s *SlackSuite) fetchSlackMessage(ctx context.Context) (slack.Msg, error) {
	var msg slack.Msg
	select {
	case data := <-s.slackServer.SeenFeed:
		err := json.Unmarshal([]byte(data), &msg)
		return msg, trace.Wrap(err)
	case <-ctx.Done():
		return msg, trace.Wrap(ctx.Err())
	}
}

func (s *SlackSuite) fetchSlackMessageAndCheck(c *C) slack.Msg {
	msg, err := s.fetchSlackMessage(s.ctx)
	c.Assert(err, IsNil)
	return msg
}

// Tests if Interactive Mode posts Slack message with buttons correctly
func (s *SlackSuite) TestSlackMessagePostingWithButtons(c *C) {
	s.startApp(c)
	request := s.createAccessRequest(c)
	pluginData := s.checkPluginData(c, request.GetName())
	msg := s.fetchSlackMessageAndCheck(c)
	c.Assert(pluginData.Timestamp, Equals, msg.Timestamp)
	c.Assert(pluginData.ChannelID, Equals, msg.Channel)
	var blockAction *slack.ActionBlock
	for _, blk := range msg.Blocks.BlockSet {
		if a, ok := blk.(*slack.ActionBlock); ok && a.BlockID == "approve_or_deny" {
			blockAction = a
		}
	}
	c.Assert(blockAction, NotNil)
	c.Assert(blockAction.Elements.ElementSet, HasLen, 2)

	c.Assert(blockAction.Elements.ElementSet[0], FitsTypeOf, &slack.ButtonBlockElement{})
	approveButton := blockAction.Elements.ElementSet[0].(*slack.ButtonBlockElement)
	c.Assert(approveButton.ActionID, Equals, "approve_request")
	c.Assert(approveButton.Value, Equals, request.GetName())

	c.Assert(blockAction.Elements.ElementSet[1], FitsTypeOf, &slack.ButtonBlockElement{})
	denyButton := blockAction.Elements.ElementSet[1].(*slack.ButtonBlockElement)
	c.Assert(denyButton.ActionID, Equals, "deny_request")
	c.Assert(denyButton.Value, Equals, request.GetName())
}

// Tests if Interactive Mode posts Slack message with buttons correctly
func (s *SlackSuite) TestSlackMessagePostingReadonly(c *C) {
	s.appConfig.Slack.NotifyOnly = true
	s.startApp(c)
	request := s.createAccessRequest(c)
	pluginData := s.checkPluginData(c, request.GetName())
	msg := s.fetchSlackMessageAndCheck(c)
	c.Assert(pluginData.Timestamp, Equals, msg.Timestamp)
	c.Assert(pluginData.ChannelID, Equals, msg.Channel)
	var blockAction *slack.ActionBlock
	for _, blk := range msg.Blocks.BlockSet {
		if a, ok := blk.(*slack.ActionBlock); ok && a.BlockID == "approve_or_deny" {
			blockAction = a
		}
	}
	// There should be no buttons block in readonly mode.
	c.Assert(blockAction, IsNil)
}

func (s *SlackSuite) TestApproval(c *C) {
	s.startApp(c)
	request := s.createAccessRequest(c)
	s.checkPluginData(c, request.GetName()) // when plugin data created, we are sure that request is completely served.

	s.postCallbackAndCheck(c, "approve_request", request.GetName(), http.StatusOK)

	request, err := s.teleport.GetAccessRequest(s.ctx, request.GetName())
	c.Assert(err, IsNil)
	c.Assert(request.GetState(), Equals, services.RequestState_APPROVED)

	auditLog, err := s.teleport.FilterAuditEvents("", events.EventFields{"event": events.AccessRequestUpdated.Name, "id": request.GetName()})
	c.Assert(err, IsNil)
	c.Assert(auditLog, HasLen, 1)
	c.Assert(auditLog[0].GetString("state"), Equals, "APPROVED")
	c.Assert(auditLog[0].GetString("delegator"), Equals, "slack:spengler@ghostbusters.example.com") // This email comes from a private const in a slacktest package
}

func (s *SlackSuite) TestDenial(c *C) {
	s.startApp(c)
	request := s.createAccessRequest(c)
	s.checkPluginData(c, request.GetName()) // when plugin data created, we are sure that request is completely served.

	s.postCallbackAndCheck(c, "deny_request", request.GetName(), http.StatusOK)

	request, err := s.teleport.GetAccessRequest(s.ctx, request.GetName())
	c.Assert(err, IsNil)
	c.Assert(request.GetState(), Equals, services.RequestState_DENIED)

	auditLog, err := s.teleport.FilterAuditEvents("", events.EventFields{"event": events.AccessRequestUpdated.Name, "id": request.GetName()})
	c.Assert(err, IsNil)
	c.Assert(auditLog, HasLen, 1)
	c.Assert(auditLog[0].GetString("state"), Equals, "DENIED")
	c.Assert(auditLog[0].GetString("delegator"), Equals, "slack:spengler@ghostbusters.example.com") // This email comes from a private const in a slacktest package
}

func (s *SlackSuite) TestApproveReadonly(c *C) {
	s.appConfig.Slack.NotifyOnly = true
	s.startApp(c)
	request := s.createAccessRequest(c)
	s.checkPluginData(c, request.GetName()) // when plugin data created, we are sure that request is completely served.
	s.postCallbackAndCheck(c, "approve_request", request.GetName(), http.StatusUnauthorized)

	request, err := s.teleport.GetAccessRequest(s.ctx, request.GetName())
	c.Assert(err, IsNil)
	c.Assert(request.GetState(), Equals, services.RequestState_PENDING)
}

func (s *SlackSuite) TestDenyReadonly(c *C) {
	s.appConfig.Slack.NotifyOnly = true
	s.startApp(c)
	request := s.createAccessRequest(c)
	s.checkPluginData(c, request.GetName()) // when plugin data created, we are sure that request is completely served.
	s.postCallbackAndCheck(c, "deny_request", request.GetName(), http.StatusUnauthorized)

	request, err := s.teleport.GetAccessRequest(s.ctx, request.GetName())
	c.Assert(err, IsNil)
	c.Assert(request.GetState(), Equals, services.RequestState_PENDING)
}

func (s *SlackSuite) TestApproveExpired(c *C) {
	s.startApp(c)
	request := s.createExpiredAccessRequest(c)
	msg1 := s.fetchSlackMessageAndCheck(c)

	s.postCallbackAndCheck(c, "approve_request", request.GetName(), http.StatusOK)

	// Get updated message
	msg2 := s.fetchSlackMessageAndCheck(c)
	c.Assert(msg1.Timestamp, Equals, msg2.Timestamp)
}

func (s *SlackSuite) TestDenyExpired(c *C) {
	s.startApp(c)
	request := s.createExpiredAccessRequest(c)
	msg1 := s.fetchSlackMessageAndCheck(c)

	s.postCallbackAndCheck(c, "deny_request", request.GetName(), http.StatusOK)

	// Get updated message
	msg2 := s.fetchSlackMessageAndCheck(c)
	c.Assert(msg1.Timestamp, Equals, msg2.Timestamp)
}
