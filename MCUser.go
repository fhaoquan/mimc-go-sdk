package mimc

import (
	"container/list"
	"encoding/json"
	"mimc-go-sdk/common/constant"
	"mimc-go-sdk/frontend"
	"mimc-go-sdk/message"
	"mimc-go-sdk/packet"
	. "mimc-go-sdk/protobuf/ims"
	. "mimc-go-sdk/protobuf/mimc"
	"mimc-go-sdk/util/byte"
	"mimc-go-sdk/util/log"
	"mimc-go-sdk/util/map"
	"mimc-go-sdk/util/queue"
	"mimc-go-sdk/util/string"
	"os"
	"os/exec"
	"strconv"
)

type UserStatus int

var logger *log.Logger = log.GetLogger(log.InfoLevel)

const (
	Online UserStatus = iota
	Offline
)

type MCUser struct {
	chid     float64
	uuid     int64
	resource string
	status   UserStatus

	isLogout bool

	clientAttrs string
	cloudAttrs  string

	appId      int64
	appAccount string
	appPackage string

	prefix  string
	indexer int

	securityKey string
	token       *string
	tryLogin    bool

	sequenceReceived        map[uint32]interface{}
	conn                    *MIMCConnection
	lastLoginTimestamp      int64
	lastCreateConnTimestamp int64
	lastPingTimestamp       int64

	tokenDelegate  Token
	statusDelegate StatusDelegate
	msgDelegate    MessageHandlerDelegate

	messageToSend *que.ConQueue
	messageToAck  *cmap.ConMap
}

func New(appId int64, appAccount string) *MCUser {
	this := NewMCUser()
	this.appId = appId
	this.appAccount = appAccount
	return this
}

func (this *MCUser) RegisterTokenDelegate(tokenDelegate Token) *MCUser {
	this.tokenDelegate = tokenDelegate
	return this
}

func (this *MCUser) RegisterStatusDelegate(statusDelegate StatusDelegate) *MCUser {
	this.statusDelegate = statusDelegate
	return this
}

func (this *MCUser) RegisterMessageDelegate(msgDelegate MessageHandlerDelegate) *MCUser {
	this.msgDelegate = msgDelegate
	return this
}

func NewMCUser() *MCUser {
	mcUser := new(MCUser)
	return mcUser
}

func (this *MCUser) InitAndSetup() {
	void := ""
	this.status = Offline
	this.resource = strutil.RandomStrWithLength(10)
	this.lastLoginTimestamp = 0
	this.lastCreateConnTimestamp = 0
	this.lastPingTimestamp = 0
	this.conn = NewConn().User(this)
	this.messageToSend = que.NewConQueue()
	this.messageToAck = cmap.NewConMap()
	this.appPackage = void
	this.chid = 0
	this.uuid = 0
	this.token = &void
	this.securityKey = void
	this.clientAttrs = void
	this.cloudAttrs = void
	this.tryLogin = false
	this.synchronizeResource()
	go this.sendRoutine()
	go this.receiveRoutine()
	go this.triggerRoutine()
}

func (this *MCUser) synchronizeResource() {
	root, _ := exec.LookPath(os.Args[0])
	dir := "/attach/"
	file := ".resource"
	key := strconv.FormatInt(this.appId, 10) + "_" + this.appAccount
	this.resource = *(strutil.SynchrnizeResource(&root, &dir, &file, &key, &(this.resource)))
}

func (this *MCUser) Login() bool {
	if this.tokenDelegate == nil {
		logger.Error("%v Login fail, have to fetch token.", this.appAccount)
		return false
	}
	tokenJsonStr := this.tokenDelegate.FetchToken()
	this.tryLogin = true
	if tokenJsonStr == nil {
		logger.Warn("%v Login fail, get nil token string.", this.appAccount)
		return false
	}
	var tokenMap map[string]interface{}
	if err := json.Unmarshal([]byte(*tokenJsonStr), &tokenMap); err == nil {
		data := tokenMap["data"].(map[string]interface{})
		code := tokenMap["code"].(float64)
		if code != 200 {
			logger.Warn("%v Login fail, response code: %v", this.appAccount, data)
			return false
		}
		this.appPackage = data["appPackage"].(string)
		this.chid = data["miChid"].(float64)
		uuid, err := strconv.ParseInt(data["miUserId"].(string), 10, 64)
		if err != nil {
			logger.Error("%v Login fail, can not parse token string.", this.appAccount)
			return false
		}
		this.uuid = uuid
		this.securityKey = data["miUserSecurityKey"].(string)
		token, ok := data["token"]
		if ok {
			tokenStr := token.(string)
			this.token = &(tokenStr)
			this.tryLogin = false
			return true
		} else {
			return false
		}
	} else {
		return false
	}
}
func (this *MCUser) Logout() bool {
	if this.status == Offline {
		return false
	}
	v6PacketForUnbind := BuildUnBindPacket(this)
	unBindPacket := msg.NewMsgPacket(cnst.MIMC_C2S_DOUBLE_DIRECTION, v6PacketForUnbind)
	this.messageToSend.Push(unBindPacket)
	return true
}

func (this *MCUser) SendMessage(toAppAccount string, msgByte []byte) string {
	if &toAppAccount == nil || msgByte == nil || len(msgByte) == 0 {
		return ""
	}
	logger.Debug("[SendMessage]%v send p2p msg to %v: %v.\n", this.appAccount, toAppAccount, string(msgByte))
	v6Packet, mimcPacket := BuildP2PMessagePacket(this, toAppAccount, msgByte, true)
	timeoutPacket := packet.NewTimeoutPacket(CurrentTimeMillis(), mimcPacket)
	msgPacket := msg.NewMsgPacket(cnst.MIMC_C2S_DOUBLE_DIRECTION, v6Packet)
	this.messageToSend.Push(msgPacket)
	this.messageToAck.Push(*(mimcPacket.PacketId), timeoutPacket)
	return *(mimcPacket.PacketId)

}

func (this *MCUser) SendGroupMessage(topicId *int64, msgByte []byte) string {
	if &topicId == nil || msgByte == nil || len(msgByte) == 0 {
		return ""
	}
	logger.Debug("[SendMessage]%v send p2t msg to %v: %v.\n", this.appAccount, *topicId, string(msgByte))
	v6Packet, mimcPacket := BuildP2TMessagePacket(this, *topicId, msgByte, true)
	timeoutPacket := packet.NewTimeoutPacket(CurrentTimeMillis(), mimcPacket)
	msgPacket := msg.NewMsgPacket(cnst.MIMC_C2S_DOUBLE_DIRECTION, v6Packet)
	this.messageToSend.Push(msgPacket)
	this.messageToAck.Push(*(mimcPacket.PacketId), timeoutPacket)
	return *(mimcPacket.PacketId)
}

func (this *MCUser) sendRoutine() {
	logger.Info("initate send goroutine.")
	if this.conn == nil {
		return
	}
	msgType := cnst.MIMC_C2S_DOUBLE_DIRECTION

	for {
		var pkt *packet.MIMCV6Packet = nil
		if this.conn.Status() == NOT_CONNECTED {
			logger.Debug("the conn not connected.\n")
			currTimeMillis := CurrentTimeMillis()
			if currTimeMillis-this.lastCreateConnTimestamp <= cnst.CONNECT_TIMEOUT {
				Sleep(100)
				continue
			}
			this.lastCreateConnTimestamp = CurrentTimeMillis()
			if !this.conn.Connect() {
				logger.Warn("connet to MIMC Server fail.\n")
				continue
			}
			this.conn.Sock_Connected()
			this.lastCreateConnTimestamp = 0
			logger.Debug("build conn packet.")
			pkt = BuildConnectionPacket(this.conn.Udid(), this)
		}
		if this.conn.Status() == SOCK_CONNECTED {
			Sleep(100)
		}
		if this.conn.Status() == HANDSHAKE_CONNECTED {
			currTimeMillis := CurrentTimeMillis()
			if this.status == Offline && currTimeMillis-this.lastLoginTimestamp <= cnst.LOGIN_TIMEOUT {
				Sleep(100)
				continue
			}
			if this.status == Offline && currTimeMillis-this.lastLoginTimestamp > cnst.LOGIN_TIMEOUT {
				logger.Debug("build bind packet.")
				pkt = BuildBindPacket(this)
				if pkt == nil {
					Sleep(100)
					continue
				}
			}
			this.lastLoginTimestamp = CurrentTimeMillis()
		}
		if this.status == Online {
			msgPacketToSend := this.messageToSend.Pop()
			if msgPacketToSend == nil {
				// 没有消息，检测ping
				curTimeMillis := CurrentTimeMillis()
				if curTimeMillis-this.lastLoginTimestamp > cnst.PING_TIMEVAL_MS {
					pkt = BuildPingPacket(this)
					logger.Debug("build ping packet. %v")
				} else {
					Sleep(100)
					continue
				}
			} else {
				msgPacket := msgPacketToSend.(*msg.MsgPacket)
				msgType = msgPacket.MsgType()
				pkt = msgPacket.Packet()
				logger.Debug("send msg packet.")

			}

		} else {
			if this.tryLogin {
				this.Login()
				Sleep(100)
			}
		}
		if pkt == nil {
			Sleep(100)
			continue
		}
		if msgType == cnst.MIMC_C2S_DOUBLE_DIRECTION {
			this.conn.TrySetNextResetSockTs()
		}
		payloadKey := PayloadKey(this.securityKey, pkt.HeaderId())
		bodyKey := this.conn.Rc4Key()
		packetData := pkt.Bytes(bodyKey, payloadKey)

		this.lastPingTimestamp = CurrentTimeMillis()
		size := len(packetData)
		if this.Conn().Writen(&packetData, size) != size {
			logger.Error("write data error.")
			this.conn.Reset()
		} else {
			if pkt.GetHeader() != nil {
				logger.Debug("[send]: send packet: %v succ.\n", *(pkt.GetHeader().Id))
			} else {
				logger.Debug("[send]: send packet succ.\n")
			}

		}
	}
}
func (this *MCUser) PeerFetcher(fetcher frontend.ProdFrontPeerFetcher) {
	this.conn.PeerFetcher(fetcher)
}
func (this *MCUser) receiveRoutine() {
	logger.Info("initate receive goroutine.\n")
	if this.conn == nil {
		return
	}
	for {
		if this.conn.Status() == NOT_CONNECTED {
			Sleep(1000)
			continue
		}
		headerBins := make([]byte, cnst.V6_HEAD_LENGTH)
		length := this.conn.Readn(&headerBins, int(cnst.V6_HEAD_LENGTH))
		if length != int(cnst.V6_HEAD_LENGTH) {
			logger.Error("[rcv]: error head. need length: %v, read lenght: %v\n", cnst.V6_HEAD_LENGTH, length)
			this.conn.Reset()
			Sleep(1000)
			continue

		}
		magic := byteutil.GetUint16FromBytes(&headerBins, cnst.V6_MAGIC_OFFSET)
		if magic != cnst.MAGIC {
			logger.Error("[rcv]: error magic.\n")
			this.conn.Reset()
			continue
		}
		version := byteutil.GetUint16FromBytes(&headerBins, cnst.V6_VERSION_OFFSET)
		if version != cnst.V6_VERSION {
			logger.Error("[rcv]: error version.\n")
			this.conn.Reset()
			continue
		}
		bodyLen := byteutil.GetIntFromBytes(&headerBins, cnst.V6_BODYLEN_OFFSET)
		if bodyLen < 0 {
			logger.Error("[rcv]: error bodylen.\n")
			this.conn.Reset()
			continue
		}
		bodyBins := make([]byte, bodyLen)
		if bodyLen != 0 {
			length = this.conn.Readn(&bodyBins, bodyLen)
			if length != bodyLen {
				logger.Error("[rcv]: error body.length: %v, bodyLen:%v", length, bodyLen)
				this.conn.Reset()
				continue
			} else {
				logger.Debug("[rcv]: read.length: %v, bodyLen:%v", length, bodyLen)
			}
		}
		crcBins := make([]byte, cnst.V6_CRC_LENGTH)
		logger.Debug("read crc")
		crclen := this.conn.Readn(&crcBins, cnst.V6_CRC_LENGTH)
		logger.Debug("read crc len: %v", crclen)
		if crclen != cnst.V6_CRC_LENGTH {
			logger.Error("[rcv]: error crc.\n")
			this.conn.Reset()
			continue
		}
		this.conn.ClearSockTimestamp()
		bodyKey := this.conn.Rc4Key()
		v6Pakcet := packet.ParseBytesToPacket(&headerBins, &bodyBins, &crcBins, bodyKey, this.securityKey)
		if v6Pakcet == nil {
			logger.Error("[rcv]: parse into v6Packet fail.")
			this.conn.Reset()
			continue
		}
		logger.Debug("[rcv]: get a packet.")
		this.handleResponse(v6Pakcet)
	}
}
func (this *MCUser) triggerRoutine() {
	logger.Info("initiate trigger goroutine.")
	if this.conn == nil {
		return
	}
	for {
		nowTimeMillis := CurrentTimeMillis()
		nextRestSockTimeMillis := this.conn.NextResetSockTimestamp()
		if nextRestSockTimeMillis > 0 && nowTimeMillis-nextRestSockTimeMillis > 0 {
			logger.Warn("[trigger] wait for response timeout.")
			this.conn.Reset()
		}
		Sleep(200)
		this.scanAndCallback()
	}
}

func (this *MCUser) scanAndCallback() {
	if this.msgDelegate == nil {
		logger.Warn("%v need to handle Message for timeout.", this.appAccount)
		return
	}
	this.messageToAck.Lock()
	defer this.messageToAck.Unlock()
	kvs := this.messageToAck.KVs()
	timeoutKeys := list.New()
	for key := range kvs {
		timeoutPacket := kvs[key].(*packet.MIMCTimeoutPacket)
		if CurrentTimeMillis()-timeoutPacket.Timestamp() < cnst.CHECK_TIMEOUT_TIMEVAL_MS {
			continue
		}
		mimcPacket := timeoutPacket.Packet()
		if *(mimcPacket.Type) == MIMC_MSG_TYPE_P2P_MESSAGE {
			p2pMessage := new(MIMCP2PMessage)
			err := Deserialize(mimcPacket.Payload, p2pMessage)
			if !err {
				return
			}
			p2pMsg := msg.NewP2pMsg(mimcPacket.PacketId, p2pMessage.From.AppAccount, p2pMessage.From.Resource, mimcPacket.Sequence, mimcPacket.Timestamp, p2pMessage.Payload)
			this.msgDelegate.HandleSendMessageTimeout(p2pMsg)
		} else if *(mimcPacket.Type) == MIMC_MSG_TYPE_P2T_MESSAGE {
			p2tMessage := new(MIMCP2TMessage)
			err := Deserialize(mimcPacket.Payload, p2tMessage)
			if !err {
				return
			}
			p2tMsg := msg.NewP2tMsg(mimcPacket.PacketId, p2tMessage.From.AppAccount, p2tMessage.From.Resource, mimcPacket.Sequence, mimcPacket.Timestamp, p2tMessage.To.TopicId, p2tMessage.Payload)
			this.msgDelegate.HandleSendGroupMessageTimeout(p2tMsg)
		}
		timeoutKeys.PushBack(key)
	}
	for ele := timeoutKeys.Front(); ele != nil; ele = ele.Next() {
		packet := this.messageToAck.Pop(ele.Value.(string))
		if packet == nil {
			logger.Warn("pop message fails. packetId: %v", ele.Value.(string))
		}
	}
}

func (this *MCUser) handleResponse(v6Packet *packet.MIMCV6Packet) {
	cmd := v6Packet.GetHeader().Cmd
	if cnst.CMD_SECMSG == *cmd {
		logger.Debug("[handleResponse] get a msg.")
		this.handleSecMsg(v6Packet)
	} else if cnst.CMD_CONN == *cmd {
		logger.Debug("[handle] conn response.")
		connResp := new(XMMsgConnResp)
		err := Deserialize(v6Packet.GetPayload(), connResp)
		if !err {
			logger.Error("[handle] parse connResp fail.")
			this.conn.Reset()
			return
		}
		this.conn.HandshakeConnected()
		logger.Debug("[handle] handshake succ.")
		this.conn.SetChallenge(*(connResp.Challenge))
		this.conn.SetChallengeAndRc4Key(*(connResp.Challenge))
	} else if cnst.CMD_BIND == *cmd {
		bindResp := new(XMMsgBindResp)
		err := Deserialize(v6Packet.GetPayload(), bindResp)
		if err {
			if *bindResp.Result {
				this.status = Online
				this.lastLoginTimestamp = 0
				logger.Debug("[handle] login succ.")
			} else {

				if cnst.MIMC_TOKEN_EXPIRE == *(bindResp.ErrorType) {
					logger.Warn("[handle] token expired, relogin().")
					this.Login()
				} else {
					this.status = Offline
					logger.Warn("[handle] login fail. %v", err)
				}

			}
			if this.statusDelegate == nil {
				logger.Warn("%v status changed, you need to handle this.", this.appAccount)
			} else {
				this.statusDelegate.HandleChange(*(bindResp.Result), bindResp.ErrorType, bindResp.ErrorReason, bindResp.ErrorDesc)
			}
		}
	} else if cnst.CMD_KICK == *cmd {
		this.status = Offline
		kick := "kick"
		logger.Debug("[handle] logout succ.")
		if this.statusDelegate == nil {
			logger.Warn("%v status changed, you need to handle this.", this.appAccount)
		} else {
			this.statusDelegate.HandleChange(false, &kick, &kick, &kick)
		}
	} else {
		return
	}
}

func (this *MCUser) handleSecMsg(v6Packet *packet.MIMCV6Packet) {
	if this.msgDelegate == nil {
		logger.Warn("%v need to regist mssage handler for received messages.", this.appAccount)
	}
	mimcPacket := new(MIMCPacket)
	err := Deserialize(v6Packet.GetPayload(), mimcPacket)
	if !err {
		logger.Warn("[handleSecMsg] unserialize mimcPacket fails.%v", err)
		return
	} else {
		switch *(mimcPacket.Type) {
		case MIMC_MSG_TYPE_PACKET_ACK:
			logger.Debug("handle Sec Msg] packet Ack.")
			packetAck := new(MIMCPacketAck)
			err := Deserialize(mimcPacket.Payload, packetAck)
			if !err {
				return
			}
			this.msgDelegate.HandleServerAck(packetAck.PacketId, packetAck.Sequence, packetAck.Timestamp)
			packet := this.messageToAck.Pop(*(packetAck.PacketId))
			if packet == nil {
				logger.Warn("pop message fails. packetId: %v", *(packetAck.PacketId))
			}
			break
		case MIMC_MSG_TYPE_COMPOUND:
			logger.Debug("[handle Sec Msg] compound.")
			packetList := new(MIMCPacketList)
			err := Deserialize(mimcPacket.Payload, packetList)
			if !err {
				return
			}
			if this.resource != *(packetList.Resource) {
				logger.Warn("Handle SecMsg MIMCPacketList resource:, current resource:", *(packetList.Resource), this.resource)
				return
			}
			seqAckPacket := BuildSequenceAckPacket(this, packetList)
			pktToSend := msg.NewMsgPacket(cnst.MIMC_C2S_SINGLE_DIRECTION, seqAckPacket)
			this.messageToSend.Push(pktToSend)
			pktNum := len(packetList.Packets)
			p2pMsgList := list.New()
			p2tMsgList := list.New()
			for i := 0; i < pktNum; i++ {
				packet := packetList.Packets[i]
				if packet == nil {
					continue
				}
				if *(packet.Type) == MIMC_MSG_TYPE_P2P_MESSAGE {
					p2pMessage := new(MIMCP2PMessage)
					err := Deserialize(packet.Payload, p2pMessage)
					if !err {
						continue
					}
					p2pMsgList.PushBack(msg.NewP2pMsg(packet.PacketId, p2pMessage.From.AppAccount, p2pMessage.From.Resource, packet.Sequence, packet.Timestamp, p2pMessage.Payload))
					continue
				} else if *(packet.Type) == MIMC_MSG_TYPE_P2T_MESSAGE {
					p2tMessage := new(MIMCP2TMessage)
					err := Deserialize(packet.Payload, p2tMessage)

					if !err {
						continue
					}
					p2tMsgList.PushBack(msg.NewP2tMsg(packet.PacketId, p2tMessage.From.AppAccount, p2tMessage.From.Resource, packet.Sequence, packet.Timestamp, p2tMessage.To.TopicId, p2tMessage.Payload))
					continue
				}
			}
			if p2pMsgList.Len() > 0 {
				this.msgDelegate.HandleMessage(p2pMsgList)
			}
			if p2tMsgList.Len() > 0 {
				this.msgDelegate.HandleGroupMessage(p2tMsgList)
			}
			break
		default:
			break
		}
	}
}
func (this *MCUser) handleToken() {
	this.token = this.tokenDelegate.FetchToken()
}

func (this *MCUser) SetResource(resource string) *MCUser {
	this.resource = resource
	return this
}
func (this *MCUser) SetUuid(uuid int64) *MCUser {
	this.uuid = uuid
	return this
}
func (this *MCUser) SetChid(chid float64) *MCUser {
	this.chid = chid
	return this
}
func (this *MCUser) SetConn(conn *MIMCConnection) *MCUser {
	this.conn = conn
	return this
}
func (this *MCUser) SetToken(token *string) *MCUser {
	this.token = token
	return this
}
func (this *MCUser) SetSecKey(secKey string) *MCUser {
	this.securityKey = secKey
	return this
}
func (this *MCUser) SetAppPackage(appPackage string) *MCUser {
	this.appPackage = appPackage
	return this
}
func (this *MCUser) SetAppAccount(appAccount string) *MCUser {
	this.appAccount = appAccount
	return this
}
func (this *MCUser) SetAppId(appId int64) *MCUser {
	this.appId = appId
	return this
}

func (this *MCUser) AppAccount() string {
	return this.appAccount
}
func (this *MCUser) AppId() int64 {
	return this.appId
}
func (this *MCUser) Conn() *MIMCConnection {
	return this.conn
}

func (this *MCUser) Uuid() int64 {
	return this.uuid
}
func (this *MCUser) Chid() float64 {
	return this.chid
}
func (this *MCUser) Resource() string {
	return this.resource
}
func (this *MCUser) SecKey() string {
	return this.securityKey
}
func (this *MCUser) Token() *string {
	return this.token
}
func (this *MCUser) ClientAttrs() string {
	return this.clientAttrs
}
func (this *MCUser) CloudAttrs() string {
	return this.cloudAttrs
}
func (this *MCUser) AppPackage() string {
	return this.appPackage
}

func (this *MCUser) Status() UserStatus {
	return this.status
}