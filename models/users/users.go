package users

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/alonsovidales/pit/log"
	"github.com/goamz/goamz/aws"
	"github.com/goamz/goamz/dynamodb"
	"golang.org/x/crypto/pbkdf2"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	cTable             = "users"
	cPrimKey           = "uid"
	cDefaultWRCapacity = 5

	CActivityAccountType = "account"
	CActivityShardsType  = "shards"
)

type ModelInt interface {
	RegisterUserPlainKey(uid string, key string, ip string) (*User, error)
	HashPassword(password string) string
	RegisterUser(uid string, key string, ip string) (user *User, err error)
	GetUserInfo(uid string, key string) (user *User)
	AdminGetUserInfoByID(uid string) (user *User)
	GetRegisteredUsers() (users map[string]*User)
}

type UsersInt interface {
	DisableUser() (persisted bool)
	EnableUser() (persisted bool)
	UpdateUser(key string) bool
	AddActivityLog(actionType string, des string, ip string) bool
	GetAllActivity() (activity map[string]*LogLine)
}

type LogLine struct {
	Ts      int64  `json:"ts"`
	Ip      string `json:"ip"`
	LogType string `json:"type"`
	Desc    string `json:"desc"`
}

type Model struct {
	ModelInt

	prefix    string
	secret    []byte
	tableName string
	conn      *dynamodb.Server
	table     *dynamodb.Table
}

type User struct {
	UsersInt `json:"-"`

	uid     string
	key     string
	Enabled string `json:"-"`
	logs    map[string][]*LogLine

	RegTs int64  `json:"reg_ts"`
	RegIp string `json:"reg_ip"`

	mutex sync.Mutex
	md    *Model
}

func GetModel(prefix string, awsRegion string) (um *Model) {
	if awsAuth, err := aws.EnvAuth(); err == nil {
		um = &Model{
			prefix:    prefix,
			tableName: fmt.Sprintf("%s_%s", prefix, cTable),
			secret:    []byte(os.Getenv("PIT_SECRET")),
			conn: &dynamodb.Server{
				Auth:   awsAuth,
				Region: aws.Regions[awsRegion],
			},
		}
		um.initTable()
	} else {
		log.Error("Problem trying to connect with DynamoDB, Error:", err)
	}

	return
}

func (um *Model) RegisterUserPlainKey(uid string, key string, ip string) (*User, error) {
	// Sanitize e-mail addr removin all the + Chars in order to avoid fake
	// duplicated accounts
	uid = strings.Replace(uid, "+", "", -1)

	if um.AdminGetUserInfoByID(uid) != nil {
		return nil, errors.New("Existing user account")
	}

	user := &User{
		uid:     uid,
		key:     key,
		Enabled: "1",
		logs:    make(map[string][]*LogLine),

		RegTs: time.Now().Unix(),
		RegIp: ip,

		md: um,
	}

	if !user.persist() {
		return nil, errors.New("Error trying to store the user data")
	}
	return user, nil
}

func (um *Model) RegisterUser(uid string, key string, ip string) (*User, error) {
	return um.RegisterUserPlainKey(uid, um.HashPassword(key), ip)
}

func (um *Model) GetUserInfo(uid string, key string) (user *User) {
	user = um.AdminGetUserInfoByID(uid)
	if user == nil || user.key != um.HashPassword(key) || user.Enabled == "0" {
		return nil
	}

	return
}

func (um *Model) AdminGetUserInfoByID(uid string) (user *User) {
	attKey := &dynamodb.Key{
		HashKey:  uid,
		RangeKey: "",
	}
	if data, err := um.table.GetItemConsistent(attKey, true); err == nil {
		user = &User{
			uid:     uid,
			key:     data["key"].Value,
			Enabled: data["enabled"].Value,
			logs:    make(map[string][]*LogLine),
			md:      um,
		}
		if err := json.Unmarshal([]byte(data["info"].Value), &user); err != nil {
			log.Error("Problem trying to retieve the user information for user:", uid, "Error:", err)
			return nil
		}
		if err = json.Unmarshal([]byte(data["logs"].Value), &user.logs); err != nil {
			log.Error("Problem trying to unmarshal the user logs for user:", uid, "Error:", err)
			return nil
		}
	}

	return
}

func (um *Model) GetRegisteredUsers() (users map[string]*User) {
	if rows, err := um.table.Scan(nil); err == nil {
		users = make(map[string]*User)
		for _, row := range rows {
			uid := row["uid"].Value
			user := &User{
				uid:     uid,
				key:     row["key"].Value,
				Enabled: row["enabled"].Value,
				logs:    make(map[string][]*LogLine),
				md:      um,
			}
			if err := json.Unmarshal([]byte(row["info"].Value), &user); err != nil {
				log.Error("Problem trying to retieve the user information for user:", user.uid, "Error:", err)
				return nil
			}
			if err = json.Unmarshal([]byte(row["logs"].Value), &user.logs); err != nil {
				log.Error("Problem trying to unmarshal the user logs for user:", user.uid, "Error:", err)
				return nil
			}
			users[uid] = user
		}
	}

	return
}

func (us *User) DisableUser() (persisted bool) {
	us.Enabled = "0"

	return us.persist()
}

func (us *User) EnableUser() (persisted bool) {
	us.Enabled = "1"

	return us.persist()
}

func (us *User) UpdateUser(key string) bool {
	us.key = us.md.HashPassword(key)

	return us.persist()
}

func (us *User) AddActivityLog(actionType string, desc, ip string) bool {
	if _, ok := us.logs[actionType]; !ok {
		us.logs[actionType] = []*LogLine{}
	}

	us.logs[actionType] = append(us.logs[actionType], &LogLine{
		Ip:      ip,
		Ts:      time.Now().Unix(),
		LogType: actionType,
		Desc:    desc,
	})

	return us.persist()
}

func (us *User) GetAllActivity() (activity map[string][]*LogLine) {
	return us.logs
}

func (um *Model) HashPassword(password string) string {
	return base64.StdEncoding.EncodeToString(pbkdf2.Key([]byte(password), um.secret, 4096, sha256.Size, sha256.New))
}

func (um *Model) delTable() {
	if tableDesc, err := um.conn.DescribeTable(um.tableName); err == nil {
		if _, err = um.conn.DeleteTable(*tableDesc); err != nil {
			log.Error("Can't remove Dynamo table:", um.tableName, "Error:", err)
		}
	} else {
		log.Error("Can't remove Dynamo table:", um.tableName, "Error:", err)
	}
}

func (us *User) persist() bool {
	userJsonInfo, _ := json.Marshal(us)
	userJsonLogs, _ := json.Marshal(us.logs)

	attribs := []dynamodb.Attribute{
		*dynamodb.NewStringAttribute(cPrimKey, us.uid),
		*dynamodb.NewStringAttribute("key", us.key),
		*dynamodb.NewStringAttribute("info", string(userJsonInfo)),
		*dynamodb.NewStringAttribute("logs", string(userJsonLogs)),
		*dynamodb.NewStringAttribute("enabled", string(us.Enabled)),
	}

	if _, err := us.md.table.PutItem(us.uid, cPrimKey, attribs); err != nil {
		log.Error("A new user can't be registered on the users table, Error:", err)

		return false
	}

	return true
}

func (um *Model) initTable() {
	pKey := dynamodb.PrimaryKey{dynamodb.NewStringAttribute(cPrimKey, ""), nil}
	um.table = um.conn.NewTable(um.tableName, pKey)

	res, err := um.table.DescribeTable()
	if err != nil {
		log.Info("Creating a new table on DynamoDB:", um.tableName)
		td := dynamodb.TableDescriptionT{
			TableName: um.tableName,
			AttributeDefinitions: []dynamodb.AttributeDefinitionT{
				dynamodb.AttributeDefinitionT{cPrimKey, "S"},
			},
			KeySchema: []dynamodb.KeySchemaT{
				dynamodb.KeySchemaT{cPrimKey, "HASH"},
			},
			ProvisionedThroughput: dynamodb.ProvisionedThroughputT{
				ReadCapacityUnits:  cDefaultWRCapacity,
				WriteCapacityUnits: cDefaultWRCapacity,
			},
		}

		if _, err := um.conn.CreateTable(td); err != nil {
			log.Error("Error trying to create a table on Dynamo DB, table:", um.tableName, "Error:", err)
		}
		if res, err = um.table.DescribeTable(); err != nil {
			log.Error("Error trying to describe a table on Dynamo DB, table:", um.tableName, "Error:", err)
		}
	}
	for "ACTIVE" != res.TableStatus {
		if res, err = um.table.DescribeTable(); err != nil {
			log.Error("Can't describe Dynamo DB instances table, Error:", err)
		}
		log.Debug("Waiting for active table, current status:", res.TableStatus)
		time.Sleep(time.Second)
	}
}
