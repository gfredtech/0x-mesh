package orderfilter

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/0xProject/0x-mesh/ethereum"
	"github.com/0xProject/0x-mesh/zeroex"
	canonicaljson "github.com/gibson042/canonicaljson-go"
	jsonschema "github.com/xeipuuv/gojsonschema"
)

const (
	pubsubTopicVersion          = 3
	topicVersionFormat          = "/0x-orders/version/%d%s"
	topicChainIDAndSchemaFormat = "/chain/%d/schema/%s"
	fullTopicFormat             = "/0x-orders/version/%d/chain/%d/schema/%s"
)

type WrongTopicVersionError struct {
	expectedVersion int
	actualVersion   int
}

func (e WrongTopicVersionError) Error() string {
	return fmt.Sprintf("wrong topic version: expected %d but got %d", e.expectedVersion, e.actualVersion)
}

var (
	// Built-in schemas
	addressSchemaLoader     = jsonschema.NewStringLoader(`{"id":"/address","type":"string","pattern":"^0x[0-9a-fA-F]{40}$"}`)
	wholeNumberSchemaLoader = jsonschema.NewStringLoader(`{"id":"/wholeNumber","anyOf":[{"type":"string","pattern":"^\\d+$"},{"type":"integer"}]}`)
	hexSchemaLoader         = jsonschema.NewStringLoader(`{"id":"/hex","type":"string","pattern":"^0x(([0-9a-fA-F][0-9a-fA-F])+)?$"}`)
	orderSchemaLoader       = jsonschema.NewStringLoader(`{"id":"/order","properties":{"makerAddress":{"$ref":"/address"},"takerAddress":{"$ref":"/address"},"makerFee":{"$ref":"/wholeNumber"},"takerFee":{"$ref":"/wholeNumber"},"senderAddress":{"$ref":"/address"},"makerAssetAmount":{"$ref":"/wholeNumber"},"takerAssetAmount":{"$ref":"/wholeNumber"},"makerAssetData":{"$ref":"/hex"},"takerAssetData":{"$ref":"/hex"},"salt":{"$ref":"/wholeNumber"},"exchangeAddress":{"$ref":"/exchangeAddress"},"feeRecipientAddress":{"$ref":"/address"},"expirationTimeSeconds":{"$ref":"/wholeNumber"}},"required":["makerAddress","takerAddress","makerFee","takerFee","senderAddress","makerAssetAmount","takerAssetAmount","makerAssetData","takerAssetData","salt","exchangeAddress","feeRecipientAddress","expirationTimeSeconds"],"type":"object"}`)
	signedOrderSchemaLoader = jsonschema.NewStringLoader(`{"id":"/signedOrder","allOf":[{"$ref":"/order"},{"properties":{"signature":{"$ref":"/hex"}},"required":["signature"]}]}`)

	// Root schemas
	rootOrderSchemaLoader = jsonschema.NewStringLoader(`{"id":"/rootOrder","allOf":[{"$ref":"/customOrder"},{"$ref":"/signedOrder"}]}`)
	// TODO(albrow): Add Topics as a required field for messages.
	rootMessageSchemaLoader = jsonschema.NewStringLoader(`{"id":"/rootMessage","properties":{"MessageType":{"type":"string"},"Order":{"$ref":"/rootOrder"}},"required":["MessageType","Order"]}`)

	// Default schema for /customOrder
	DefaultCustomOrderSchema = `{}`
)

var builtInSchemas = []jsonschema.JSONLoader{
	addressSchemaLoader,
	wholeNumberSchemaLoader,
	hexSchemaLoader,
	orderSchemaLoader,
	signedOrderSchemaLoader,
}

type Filter struct {
	topic                string
	version              int
	chainID              int
	rawCustomOrderSchema string
	orderSchema          *jsonschema.Schema
	messageSchema        *jsonschema.Schema
}

func New(chainID int, customOrderSchema string) (*Filter, error) {
	orderLoader, err := newLoader(chainID, customOrderSchema)
	rootOrderSchema, err := orderLoader.Compile(rootOrderSchemaLoader)
	if err != nil {
		return nil, err
	}

	messageLoader, err := newLoader(chainID, customOrderSchema)
	if err := messageLoader.AddSchemas(rootOrderSchemaLoader); err != nil {
		return nil, err
	}
	rootMessageSchema, err := messageLoader.Compile(rootMessageSchemaLoader)
	if err != nil {
		return nil, err
	}
	return &Filter{
		chainID:              chainID,
		rawCustomOrderSchema: customOrderSchema,
		orderSchema:          rootOrderSchema,
		messageSchema:        rootMessageSchema,
	}, nil
}

func loadExchangeAddress(loader *jsonschema.SchemaLoader, chainID int) error {
	contractAddresses, err := ethereum.GetContractAddressesForChainID(chainID)
	if err != nil {
		return err
	}
	// Note that exchangeAddressSchema accepts both checksummed and
	// non-checksummed (i.e. all lowercase) addresses.
	exchangeAddressSchema := fmt.Sprintf(`{"oneOf":[{"type":"string","pattern":%q},{"type":"string","pattern":%q}]}`, contractAddresses.Exchange.Hex(), strings.ToLower(contractAddresses.Exchange.Hex()))
	return loader.AddSchema("/exchangeAddress", jsonschema.NewStringLoader(exchangeAddressSchema))
}

func newLoader(chainID int, customOrderSchema string) (*jsonschema.SchemaLoader, error) {
	loader := jsonschema.NewSchemaLoader()
	if err := loadExchangeAddress(loader, chainID); err != nil {
		return nil, err
	}
	if err := loader.AddSchemas(builtInSchemas...); err != nil {
		return nil, err
	}
	if err := loader.AddSchema("/customOrder", jsonschema.NewStringLoader(customOrderSchema)); err != nil {
		return nil, err
	}
	return loader, nil
}

func NewFromTopic(topic string) (*Filter, error) {
	// TODO(albrow): Use a cache for topic -> filter
	var version int
	var chainIDAndSchema string
	if _, err := fmt.Sscanf(topic, topicVersionFormat, &version, &chainIDAndSchema); err != nil {
		return nil, fmt.Errorf("could not parse topic version for topic: %q", topic)
	}
	if version != pubsubTopicVersion {
		return nil, WrongTopicVersionError{
			expectedVersion: pubsubTopicVersion,
			actualVersion:   version,
		}
	}
	var chainID int
	var base64EncodedSchema string
	if _, err := fmt.Sscanf(chainIDAndSchema, topicChainIDAndSchemaFormat, &chainID, &base64EncodedSchema); err != nil {
		return nil, fmt.Errorf("could not parse chainID and schema from topic: %q", topic)
	}
	customOrderSchema, err := base64.URLEncoding.DecodeString(base64EncodedSchema)
	if err != nil {
		return nil, fmt.Errorf("could not base64-decode order schema: %q", base64EncodedSchema)
	}
	return New(chainID, string(customOrderSchema))
}

func (f *Filter) Topic() string {
	if f.topic == "" {
		f.topic = f.generateTopic()
	}
	return f.topic
}

func (v *Filter) generateTopic() string {
	var holder interface{} = struct{}{}
	_ = canonicaljson.Unmarshal([]byte(v.rawCustomOrderSchema), &holder)
	canonicalOrderSchemaJSON, _ := canonicaljson.Marshal(holder)
	base64EncodedSchema := base64.URLEncoding.EncodeToString(canonicalOrderSchemaJSON)
	return fmt.Sprintf(fullTopicFormat, pubsubTopicVersion, v.chainID, base64EncodedSchema)
}

func (f *Filter) MatchMessageJSON(messageJSON []byte) (bool, error) {
	result, err := f.messageSchema.Validate(jsonschema.NewBytesLoader(messageJSON))
	if err != nil {
		return false, err
	}
	return result.Valid(), nil
}

func (f *Filter) ValidateOrderJSON(orderJSON []byte) (*jsonschema.Result, error) {
	return f.orderSchema.Validate(jsonschema.NewBytesLoader(orderJSON))
}

func (f *Filter) ValidateOrder(order *zeroex.SignedOrder) (*jsonschema.Result, error) {
	return f.orderSchema.Validate(jsonschema.NewGoLoader(order))
}