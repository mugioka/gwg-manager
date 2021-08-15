package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	gcache "github.com/patrickmn/go-cache"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	cloudidentity "google.golang.org/api/cloudidentity/v1beta1"
)

const (
	addUserBlockID              = "add-user"
	selectUserActionID          = "select-user"
	submitSelectingUserActionID = "submit-selecting-user"
	selectGroupActionID         = "select-group"
	selectExpirationActionID    = "select-expiration"
	submitAddingUserActionID    = "submit-adding-user"
	allowAddingUserActionID     = "accept-adding-user"
	denyAddingUserActionID      = "deny-adding-user"
	cancelActionID              = "cancel"
	AttachmentGoodColor         = "good"
	AttachmentDangerColor       = "danger"
	cacheKeyGroups              = "groups"
)

var (
	appToken              string
	botToken              string
	orgCustomerID         string
	approverGroupID       string
	groupCache            *gcache.Cache
	memberShipCache       *gcache.Cache
	cloudidentityClient   *cloudidentity.Service
	slackAPI              *slack.Client
	slackClient           *socketmode.Client
	selectableExpirations = []map[string]string{
		{"value": "1", "displayValue": "1h"},
		{"value": "6", "displayValue": "6h"},
		{"value": "12", "displayValue": "12h"},
		{"value": "24", "displayValue": "24h"},
	}
)

type requestMemberShip struct {
	AddingUserEmail string `json:"addingUserEmail"`
	AddingUserID    string `json:"addingUserID"`
	GroupID         string `json:"groupID"`
	GroupName       string `json:"groupName"`
	Expiration      int    `json:"expiration,string"`
	RequestedUserID string `json:"requestedUserID"`
}

type cacheGroup struct {
	name        string
	displayName string
	memberShips []*cloudidentity.Membership
}

func main() {
	if err := initEnv(); err != nil {
		log.Fatal(err.Error())
	}
	if err := initClinets(); err != nil {
		log.Fatal(err.Error())
	}
	go getGroups()
	go func() {
		for evt := range slackClient.Events {
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					fmt.Printf("Ignored %+v\n", evt)

					continue
				}
				slackClient.Ack(*evt.Request)
				switch eventsAPIEvent.Type {
				case slackevents.CallbackEvent:
					innerEvent := eventsAPIEvent.InnerEvent
					switch ev := innerEvent.Data.(type) {
					case *slackevents.AppMentionEvent:
						var err error
						m := strings.Split(strings.TrimSpace(ev.Text), " ")[1:]
						if len(m) < 2 {
							return
						}
						switch m[0] {
						case "add":
							switch m[1] {
							case "member":
								err = postMsgForSelectingUser(ev.Channel, ev.User)
							}
						case "remove":
							switch m[1] {
							case "member":

							}
						case "list":
							switch m[1] {
							case "group":
							case "member":
							}
						}
						if err != nil {
							slackClient.Debugf(err.Error())
						}
					}
				default:
					slackClient.Debugf("unsupported Events API event received")
				}
			case socketmode.EventTypeInteractive:
				callback, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					fmt.Printf("Ignored %+v\n", evt)
					continue
				}
				slackClient.Ack(*evt.Request)
				switch callback.Type {
				case slack.InteractionTypeBlockActions:
					action := callback.ActionCallback.BlockActions[len(callback.ActionCallback.BlockActions)-1]
					switch action.ActionID {
					case cancelActionID:
						attachment := slack.Attachment{
							Text:  "Successfully cancelled :white_check_mark:",
							Color: AttachmentGoodColor,
						}
						_, _, err := slackAPI.PostMessage("", slack.MsgOptionReplaceOriginal(callback.ResponseURL), slack.MsgOptionAttachments(attachment))
						if err != nil {
							slackClient.Debugf(err.Error())
							return
						}
					case submitSelectingUserActionID:
						selectedUserID := callback.BlockActionState.Values[addUserBlockID][selectUserActionID].SelectedUser
						if selectedUserID == "" {
							attachment := slack.Attachment{
								Text:  "Must be selected before submission :warning:",
								Color: AttachmentDangerColor,
							}
							err := postMsgForSelectingUser(callback.CallbackID, callback.User.ID, slack.MsgOptionReplaceOriginal(callback.ResponseURL), slack.MsgOptionAttachments(attachment))
							if err != nil {
								slackClient.Debugf(err.Error())
								return
							}
						}
						u, err := slackAPI.GetUserInfo(selectedUserID)
						if err != nil {
							slackClient.Debugf(err.Error())
							return
						}
						err = postMsgForSelectingGroupAndExpiration(callback.Channel.ID, callback.User.ID, u, slack.MsgOptionReplaceOriginal(callback.ResponseURL))
						if err != nil {
							slackClient.Debugf(err.Error())
							return
						}
					case submitAddingUserActionID:
						groupID := callback.BlockActionState.Values[addUserBlockID][selectGroupActionID].SelectedOption.Value
						expiration := callback.BlockActionState.Values[addUserBlockID][selectExpirationActionID].SelectedOption.Value
						addingUser, err := slackAPI.GetUserByEmail(action.Value)
						if err != nil {
							slackClient.Debugf(err.Error())
							return
						}
						if groupID == "" || expiration == "" {
							attachment := slack.Attachment{
								Text:  "Must be selected before submission :warning:",
								Color: AttachmentDangerColor,
							}
							err = postMsgForSelectingGroupAndExpiration(callback.Channel.ID, callback.User.ID, addingUser, slack.MsgOptionAttachments(attachment), slack.MsgOptionReplaceOriginal(callback.ResponseURL))
							if err != nil {
								slackClient.Debugf(err.Error())
								return
							}
						}
						groupName := callback.BlockActionState.Values[addUserBlockID][selectGroupActionID].SelectedOption.Text.Text
						expirationTime := callback.BlockActionState.Values[addUserBlockID][selectExpirationActionID].SelectedOption.Text.Text
						buttonValue := fmt.Sprintf(`{"addingUserEmail":"%s","addingUserID":"%s","groupID":"%s","groupName":"%s","expiration":"%s","requestedUserID":"%s"}`, addingUser.Profile.Email, addingUser.ID, groupID, groupName, expiration, callback.User.ID)
						_, _, err = slackAPI.PostMessage(callback.Channel.ID, slack.MsgOptionBlocks(
							slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, fmt.Sprintf("Requested from <@%s>.\n<!subteam^%s> allows <@%s> to join the `%s` group with an expiration time of `%s`?", callback.User.ID, approverGroupID, addingUser.ID, groupName, expirationTime), false, false), nil, nil),
							slack.NewActionBlock(
								addUserBlockID,
								slack.NewButtonBlockElement(allowAddingUserActionID, buttonValue, slack.NewTextBlockObject(slack.PlainTextType, "Allow", false, false)).WithStyle(slack.StylePrimary),
								slack.NewButtonBlockElement(denyAddingUserActionID, buttonValue, slack.NewTextBlockObject(slack.PlainTextType, "Deny", false, false)).WithStyle(slack.StyleDanger),
							),
						))
						if err != nil {
							slackClient.Debugf(err.Error())
							return
						}
						attachment := slack.Attachment{
							Text:  fmt.Sprintf("Your request has been successfully sent. It is awaiting approval from <@%s> :white_check_mark:", callback.User.ID),
							Color: AttachmentGoodColor,
						}
						_, err = slackAPI.PostEphemeral(callback.Channel.ID, callback.User.ID, slack.MsgOptionAttachments(attachment), slack.MsgOptionPostEphemeral(callback.User.ID), slack.MsgOptionReplaceOriginal(callback.ResponseURL))
						if err != nil {
							slackClient.Debugf(err.Error())
							return
						}
					case allowAddingUserActionID:
						_, _, err := slackAPI.PostMessage(callback.Channel.ID, slack.MsgOptionDeleteOriginal(callback.ResponseURL))
						if err != nil {
							slackClient.Debugf(err.Error())
							return
						}
						message, isErr := addUserToGroup(action, callback.User.ID)
						var color string
						if isErr {
							color = AttachmentDangerColor
						} else {
							color = AttachmentGoodColor
						}
						attachment := slack.Attachment{
							Text:  message,
							Color: color,
						}
						_, _, err = slackAPI.PostMessage(callback.Channel.ID, slack.MsgOptionAttachments(attachment))
						if err != nil {
							slackClient.Debugf(err.Error())
							return
						}
					case denyAddingUserActionID:
						_, _, err := slackAPI.PostMessage(callback.Channel.ID, slack.MsgOptionDeleteOriginal(callback.ResponseURL))
						if err != nil {
							slackClient.Debugf(err.Error())
							return
						}
						var rms requestMemberShip
						err = json.Unmarshal([]byte(action.Value), &rms)
						if err != nil {
							slackClient.Debugf(err.Error())
							return
						}
						attachment := slack.Attachment{
							Text:  fmt.Sprintf("Request of <@%s> is denied by <@%s>.\n<@%s> did not join `%s` group.", rms.RequestedUserID, callback.User.ID, rms.AddingUserID, rms.GroupName),
							Color: AttachmentDangerColor,
						}
						_, _, err = slackAPI.PostMessage(callback.Channel.ID, slack.MsgOptionAttachments(attachment))
						if err != nil {
							slackClient.Debugf(err.Error())
							return
						}
					}
				}
			}
		}
	}()
	go func() {
		// Determine port for HTTP service.
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
			fmt.Printf("defaulting to port %s", port)
		}
		// Start HTTP server for HC only.
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			fmt.Print(err.Error())
		}
	}()

	if err := slackClient.Run(); err != nil {
		fmt.Print(err.Error())
	}
}

func initEnv() error {
	appToken = os.Getenv("SLACK_APP_TOKEN")
	if !strings.HasPrefix(appToken, "xapp-") {
		return fmt.Errorf("SLACK_APP_TOKEN must have the prefix \"xapp-\".")
	}

	botToken = os.Getenv("SLACK_BOT_TOKEN")
	if botToken == "" {
		return fmt.Errorf("SLACK_BOT_TOKEN must be set.\n")
	}
	if !strings.HasPrefix(botToken, "xoxb-") {
		return fmt.Errorf("SLACK_BOT_TOKEN must have the prefix \"xoxb-\".")
	}
	orgCustomerID = os.Getenv("ORG_CUSTOMER_ID")
	if orgCustomerID == "" {
		return fmt.Errorf("ORG_CUSTOMER_ID must be set.\n")
	}
	approverGroupID = os.Getenv("APPROVER_GROUP_ID")
	if approverGroupID == "" {
		return fmt.Errorf("APPROVER_GROUP_ID must be set.\n")
	}
	return nil
}

func initClinets() error {
	slackAPI = slack.New(
		botToken,
		slack.OptionDebug(true),
		slack.OptionLog(log.New(os.Stdout, "api: ", log.Lshortfile|log.LstdFlags)),
		slack.OptionAppLevelToken(appToken),
	)

	slackClient = socketmode.New(
		slackAPI,
		socketmode.OptionDebug(true),
		socketmode.OptionLog(log.New(os.Stdout, "socketmode: ", log.Lshortfile|log.LstdFlags)),
	)

	groupCache = gcache.New(gcache.NoExpiration, 0)

	memberShipCache = gcache.New(gcache.NoExpiration, 1*time.Minute)
	memberShipCache.OnEvicted(removeMemberShipOnCacheEvicted)

	ctx := context.Background()
	var err error
	if cloudidentityClient, err = cloudidentity.NewService(ctx); err != nil {
		return err
	}
	return nil
}

func removeMemberShipOnCacheEvicted(key string, v interface{}) {
	ms := v.(*cloudidentity.Membership)
	o, err := cloudidentityClient.Groups.Memberships.Delete(ms.Name).Do()
	if err != nil {
		slackClient.Debugln(err.Error())
		return
	}
	for {
		if o.Done {
			if o.Error != nil {
				slackClient.Debugln(o.Error.Message)
				return
			}
			slackClient.Debugf("%+v is removed.", ms)
			break
		}
	}
}

func getGroups() {
	for {
		var cacheGroups []cacheGroup
		res, err := cloudidentityClient.Groups.List().Parent(fmt.Sprintf("customers/%s", orgCustomerID)).Do()
		if err != nil {
			fmt.Print(err.Error())
			return
		}
		for _, g := range res.Groups {
			res, err := cloudidentityClient.Groups.Memberships.List(g.Name).Do()
			if err != nil {
				fmt.Println(err.Error())
				return
			}
			cacheGroups = append(cacheGroups, cacheGroup{name: g.Name, displayName: g.DisplayName, memberShips: res.Memberships})
		}
		groupCache.Set(cacheKeyGroups, &cacheGroups, gcache.DefaultExpiration)
		time.Sleep(1 * time.Minute)
	}
}

func postMsgForSelectingUser(chID string, userID string, msgOps ...slack.MsgOption) error {
	UserSelectionMenu := slack.NewOptionsSelectBlockElement(
		slack.OptTypeUser,
		slack.NewTextBlockObject(slack.PlainTextType, "Select an user", false, false),
		selectUserActionID,
	)
	UserSelectionMenu.InitialUser = userID
	_, _, err := slackAPI.PostMessage(
		chID,
		slack.MsgOptionBlocks(
			*slack.NewActionBlock(
				addUserBlockID,
				UserSelectionMenu,
				slack.NewButtonBlockElement(submitSelectingUserActionID, "", slack.NewTextBlockObject(slack.PlainTextType, "submit", false, false)).WithStyle(slack.StylePrimary),
				slack.NewButtonBlockElement(cancelActionID, "", slack.NewTextBlockObject(slack.PlainTextType, cancelActionID, false, false)).WithStyle(slack.StyleDanger),
			),
		),
		slack.MsgOptionPostEphemeral(userID),
		slack.MsgOptionCompose(msgOps...),
	)
	if err != nil {
		return err
	}
	return nil
}

func postMsgForSelectingGroupAndExpiration(chID string, userID string, addingUser *slack.User, msgOps ...slack.MsgOption) error {
	groupObjects, err := buildSelectableGroupObjects(addingUser.Profile.Email)
	if err != nil {
		return err
	}
	_, _, err = slackAPI.PostMessage(
		chID,
		slack.MsgOptionBlocks(
			slack.NewActionBlock(
				addUserBlockID,
				slack.NewOptionsSelectBlockElement(
					slack.OptTypeStatic,
					slack.NewTextBlockObject(slack.PlainTextType, "Select a group", false, false),
					selectGroupActionID,
					groupObjects...,
				),
				slack.NewOptionsSelectBlockElement(
					slack.OptTypeStatic,
					slack.NewTextBlockObject(slack.PlainTextType, "Select an expiration", false, false),
					selectExpirationActionID,
					buildExpirationObjects()...,
				),
				slack.NewButtonBlockElement(submitAddingUserActionID, addingUser.Profile.Email, slack.NewTextBlockObject(slack.PlainTextType, "submit", false, false)).WithStyle(slack.StylePrimary),
				slack.NewButtonBlockElement(cancelActionID, "", slack.NewTextBlockObject(slack.PlainTextType, cancelActionID, false, false)).WithStyle(slack.StyleDanger),
			),
		),
		slack.MsgOptionPostEphemeral(userID),
		slack.MsgOptionCompose(msgOps...),
	)
	if err != nil {
		return err
	}
	return nil
}

func buildSelectableGroupObjects(userEmail string) ([]*slack.OptionBlockObject, error) {
	var objects []*slack.OptionBlockObject
	if x, found := groupCache.Get(cacheKeyGroups); found {
		groups := x.(*[]cacheGroup)
		for _, g := range *groups {
			if !isUserAlreadyExistedInGroup(userEmail, g.memberShips) {
				o := slack.NewOptionBlockObject(g.name, slack.NewTextBlockObject(slack.PlainTextType, g.displayName, false, false), nil)
				objects = append(objects, o)
			}
		}
	}
	return objects, nil
}

func isUserAlreadyExistedInGroup(userEmail string, mss []*cloudidentity.Membership) bool {
	for _, ms := range mss {
		if userEmail == ms.MemberKey.Id {
			return true
		}
	}
	return false
}

func buildExpirationObjects() []*slack.OptionBlockObject {
	var objects []*slack.OptionBlockObject
	for _, e := range selectableExpirations {
		o := slack.NewOptionBlockObject(e["value"], slack.NewTextBlockObject(slack.PlainTextType, e["displayValue"], false, false), nil)
		objects = append(objects, o)
	}
	return objects
}

func addUserToGroup(blockAction *slack.BlockAction, approvedUserID string) (string, bool) {
	var rms requestMemberShip
	err := json.Unmarshal([]byte(blockAction.Value), &rms)
	if err != nil {
		return fmt.Sprintf("JSON unmarshal error.\nError detail: %s.", err.Error()), true
	}
	ms := &cloudidentity.Membership{
		PreferredMemberKey: &cloudidentity.EntityKey{
			Id: rms.AddingUserEmail,
		},
		Roles: []*cloudidentity.MembershipRole{
			{Name: "MEMBER"},
		},
	}
	o, err := cloudidentityClient.Groups.Memberships.Create(rms.GroupID, ms).Do()
	if err != nil {
		return fmt.Sprintf("Request of <@%s> has been approved by <@%s>, but it failed with an error.\n%s.", rms.RequestedUserID, approvedUserID, err.Error()), true
	}
	for {
		if o.Done {
			if o.Error != nil {
				return fmt.Sprintf("Request of <@%s> has been approved by <@%s>, but it failed with an error.\n%s.", rms.RequestedUserID, approvedUserID, err.Error()), true
			}
			ms := &cloudidentity.Membership{}
			err := json.Unmarshal(o.Response, ms)
			if err != nil {
				return err.Error(), true
			}
			memberShipCache.Set(ms.Name, ms, time.Duration(rms.Expiration)*time.Hour)
			return fmt.Sprintf("Request of <@%s> has been approved by <@%s> and processed successfully.\n<@%s> is joined `%s` group.", rms.RequestedUserID, approvedUserID, rms.AddingUserID, rms.GroupName), false
		}
	}
}
