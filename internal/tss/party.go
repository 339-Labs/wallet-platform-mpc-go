package tss

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"sync"

	"github.com/bnb-chain/tss-lib/v2/common"
	"github.com/bnb-chain/tss-lib/v2/ecdsa/keygen"
	"github.com/bnb-chain/tss-lib/v2/ecdsa/signing"
	"github.com/bnb-chain/tss-lib/v2/tss"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/sirupsen/logrus"
)

// LocalParty 本地参与方
type LocalParty struct {
	PartyID   *tss.PartyID
	Params    *tss.Parameters
	LocalData *keygen.LocalPartySaveData
	Key       *big.Int
	Index     int
}

// PartyIDGenerator 参与方ID生成器
type PartyIDGenerator struct {
	mu      sync.Mutex
	counter int
}

var partyIDGen = &PartyIDGenerator{}

// GeneratePartyID 生成参与方ID
func GeneratePartyID(nodeID string, index int) *tss.PartyID {
	// 使用节点ID的哈希作为key
	keyBytes := crypto.Keccak256([]byte(nodeID))
	key := new(big.Int).SetBytes(keyBytes)

	return tss.NewPartyID(nodeID, nodeID, key)
}

// CreatePartyIDs 创建参与方ID列表
func CreatePartyIDs(nodeIDs []string) tss.SortedPartyIDs {
	var partyIDs []*tss.PartyID

	for i, nodeID := range nodeIDs {
		pid := GeneratePartyID(nodeID, i)
		partyIDs = append(partyIDs, pid)
	}

	// 按key排序
	sortedIDs := tss.SortPartyIDs(partyIDs)
	return sortedIDs
}

// GetPartyIndex 获取参与方在排序列表中的索引
func GetPartyIndex(sortedPartyIDs tss.SortedPartyIDs, nodeID string) int {
	for i, pid := range sortedPartyIDs {
		if pid.Id == nodeID {
			return i
		}
	}
	return -1
}

// CreateKeygenParams 创建密钥生成参数
// threshold 参数表示签名所需的最少节点数（用户视角）
// TSS 库内部使用的 threshold 是 t，其中 t+1 = 用户threshold
func CreateKeygenParams(partyIDs tss.SortedPartyIDs, partyID *tss.PartyID, threshold, total int) *tss.Parameters {
	// 确保partyID在列表中
	var ourPartyID *tss.PartyID
	for _, pid := range partyIDs {
		if pid.Id == partyID.Id {
			ourPartyID = pid
			break
		}
	}

	if ourPartyID == nil {
		logrus.Error("Our party ID not found in party list")
		return nil
	}

	ctx := tss.NewPeerContext(partyIDs)
	// 用户的 threshold 表示签名所需节点数，TSS 库需要 threshold-1
	tssThreshold := threshold - 1
	params := tss.NewParameters(tss.S256(), ctx, ourPartyID, len(partyIDs), tssThreshold)

	return params
}

// CreateSigningParams 创建签名参数
// threshold 参数表示签名所需的最少节点数（用户视角）
// TSS 库内部使用的 threshold 是 t，其中 t+1 = 用户threshold
func CreateSigningParams(partyIDs tss.SortedPartyIDs, partyID *tss.PartyID, threshold int) *tss.Parameters {
	var ourPartyID *tss.PartyID
	for _, pid := range partyIDs {
		if pid.Id == partyID.Id {
			ourPartyID = pid
			break
		}
	}

	if ourPartyID == nil {
		logrus.Error("Our party ID not found in party list")
		return nil
	}

	ctx := tss.NewPeerContext(partyIDs)
	// 用户的 threshold 表示签名所需节点数，TSS 库需要 threshold-1
	tssThreshold := threshold - 1
	params := tss.NewParameters(tss.S256(), ctx, ourPartyID, len(partyIDs), tssThreshold)

	return params
}

// PublicKeyToAddress 从公钥获取以太坊地址
func PublicKeyToAddress(pubKey *ecdsa.PublicKey) string {
	return crypto.PubkeyToAddress(*pubKey).Hex()
}

// PublicKeyToHex 公钥转十六进制
func PublicKeyToHex(pubKey *ecdsa.PublicKey) string {
	pubKeyBytes := crypto.FromECDSAPub(pubKey)
	return hex.EncodeToString(pubKeyBytes)
}

// GetPublicKeyFromLocalData 从本地数据获取公钥
func GetPublicKeyFromLocalData(localData *keygen.LocalPartySaveData) (*ecdsa.PublicKey, error) {
	pkX, pkY := localData.ECDSAPub.X(), localData.ECDSAPub.Y()

	pubKey := &ecdsa.PublicKey{
		Curve: tss.S256(),
		X:     pkX,
		Y:     pkY,
	}

	return pubKey, nil
}

// RecoverPublicKey 从签名数据重建公钥
func RecoverPublicKey(localData *keygen.LocalPartySaveData) (*ecdsa.PublicKey, string, error) {
	pubKey, err := GetPublicKeyFromLocalData(localData)
	if err != nil {
		return nil, "", err
	}

	address := PublicKeyToAddress(pubKey)
	return pubKey, address, nil
}

// MessageWrapper 消息包装器
type MessageWrapper struct {
	From        string `json:"from"`
	To          string `json:"to"`
	IsBroadcast bool   `json:"is_broadcast"`
	Data        []byte `json:"data"`
	Round       int    `json:"round"`
}

// SerializeMessage 序列化TSS消息
func SerializeMessage(msg tss.Message) (*MessageWrapper, error) {
	data, routing, err := msg.WireBytes()
	if err != nil {
		return nil, fmt.Errorf("failed to get wire bytes: %w", err)
	}

	wrapper := &MessageWrapper{
		From:        msg.GetFrom().Id,
		IsBroadcast: routing.IsBroadcast,
		Data:        data,
	}

	if !routing.IsBroadcast && len(routing.To) > 0 {
		wrapper.To = routing.To[0].Id
	}

	return wrapper, nil
}

// DeserializeMessage 反序列化TSS消息
func DeserializeMessage(wrapper *MessageWrapper, partyIDs tss.SortedPartyIDs) (tss.ParsedMessage, error) {
	// 查找发送方的PartyID
	var fromPartyID *tss.PartyID
	for _, pid := range partyIDs {
		if pid.Id == wrapper.From {
			fromPartyID = pid
			break
		}
	}

	if fromPartyID == nil {
		return nil, fmt.Errorf("sender party not found: %s", wrapper.From)
	}

	// 解析消息
	msg, err := tss.ParseWireMessage(wrapper.Data, fromPartyID, wrapper.IsBroadcast)
	if err != nil {
		return nil, fmt.Errorf("failed to parse wire message: %w", err)
	}

	return msg, nil
}

// SignatureData 签名数据
type SignatureData struct {
	R *big.Int
	S *big.Int
	V int
}

// GetSignature 从签名数据获取签名
func GetSignature(signData *common.SignatureData) *SignatureData {
	return &SignatureData{
		R: new(big.Int).SetBytes(signData.R),
		S: new(big.Int).SetBytes(signData.S),
		V: int(signData.SignatureRecovery[0]),
	}
}

// SignatureToBytes 签名转字节
func SignatureToBytes(sig *SignatureData) []byte {
	rBytes := sig.R.Bytes()
	sBytes := sig.S.Bytes()

	// 确保r和s都是32字节
	r := make([]byte, 32)
	s := make([]byte, 32)
	copy(r[32-len(rBytes):], rBytes)
	copy(s[32-len(sBytes):], sBytes)

	// 组合签名: r || s || v
	signature := make([]byte, 65)
	copy(signature[:32], r)
	copy(signature[32:64], s)
	signature[64] = byte(sig.V)

	return signature
}

// SignatureToHex 签名转十六进制
func SignatureToHex(sig *SignatureData) string {
	return hex.EncodeToString(SignatureToBytes(sig))
}

// PrepareKeysForSigning 准备签名所需的密钥数据
func PrepareKeysForSigning(localData *keygen.LocalPartySaveData, signerIDs []string) (*keygen.LocalPartySaveData, error) {
	// 对签名者ID进行排序
	sortedSignerIDs := make([]string, len(signerIDs))
	copy(sortedSignerIDs, signerIDs)
	sort.Strings(sortedSignerIDs)

	// 创建新的本地数据用于签名
	newLocalData := *localData

	return &newLocalData, nil
}

// GetOriginalIndex 获取原始索引
func GetOriginalIndex(localData *keygen.LocalPartySaveData, nodeID string) (int, error) {
	for i, id := range localData.Ks {
		if id.String() == nodeID || id.Cmp(crypto.Keccak256Hash([]byte(nodeID)).Big()) == 0 {
			return i, nil
		}
	}
	return -1, fmt.Errorf("party not found in key data")
}

// BuildSigningPartyIDs 构建签名参与方ID
func BuildSigningPartyIDs(localData *keygen.LocalPartySaveData, signerNodeIDs []string) (tss.SortedPartyIDs, error) {
	partyIDs := CreatePartyIDs(signerNodeIDs)
	return partyIDs, nil
}

// NewKeygenParty 创建密钥生成参与方
func NewKeygenParty(params *tss.Parameters, outChan chan tss.Message, endChan chan *keygen.LocalPartySaveData) *keygen.LocalParty {
	return keygen.NewLocalParty(params, outChan, endChan).(*keygen.LocalParty)
}

// NewSigningParty 创建签名参与方
func NewSigningParty(msg *big.Int, params *tss.Parameters, key *keygen.LocalPartySaveData, outChan chan tss.Message, endChan chan *common.SignatureData) *signing.LocalParty {
	return signing.NewLocalParty(msg, params, *key, outChan, endChan).(*signing.LocalParty)
}
