package downloader

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"
)

const xmlrpcMaxBody = 8 << 20

// xmlrpcValue 是 rTorrent XML-RPC 的通用值（string/int/double/boolean/array/struct）。
type xmlrpcValue struct {
	String *string
	Int    *int64
	Double *float64
	Bool   *bool
	Array  []xmlrpcValue
	Struct map[string]xmlrpcValue
}

type xmlrpcMember struct {
	Name  string      `xml:"name"`
	Value xmlrpcValue `xml:"value"`
}

func (v *xmlrpcValue) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	if start.Name.Local == "value" {
		var wrapper struct {
			Inner *xmlrpcValue `xml:",any"`
		}
		if err := decoder.DecodeElement(&wrapper, &start); err != nil {
			return err
		}
		if wrapper.Inner != nil {
			*v = *wrapper.Inner
		}
		return nil
	}
	switch start.Name.Local {
	case "string", "base64":
		var value string
		if err := decoder.DecodeElement(&value, &start); err != nil {
			return err
		}
		v.String = &value
	case "i4", "int":
		var value int64
		if err := decoder.DecodeElement(&value, &start); err != nil {
			return err
		}
		v.Int = &value
	case "double":
		var value float64
		if err := decoder.DecodeElement(&value, &start); err != nil {
			return err
		}
		v.Double = &value
	case "boolean":
		var value bool
		if err := decoder.DecodeElement(&value, &start); err != nil {
			return err
		}
		v.Bool = &value
	case "array":
		var wrapper struct {
			Values []xmlrpcValue `xml:"data>value"`
		}
		if err := decoder.DecodeElement(&wrapper, &start); err != nil {
			return err
		}
		v.Array = wrapper.Values
	case "struct":
		var wrapper struct {
			Members []xmlrpcMember `xml:"member"`
		}
		if err := decoder.DecodeElement(&wrapper, &start); err != nil {
			return err
		}
		v.Struct = make(map[string]xmlrpcValue, len(wrapper.Members))
		for _, member := range wrapper.Members {
			v.Struct[member.Name] = member.Value
		}
	default:
		return decoder.Skip()
	}
	return nil
}

func xmlrpcString(value string) string {
	return "<value><string>" + escapeXML(value) + "</string></value>"
}

func escapeXML(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}

// xmlrpcCall 发送一次 rTorrent XML-RPC 调用并返回第一个参数值。
func xmlrpcCall(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	method string,
	params []string,
	username, password string,
) (xmlrpcValue, error) {
	if ctx == nil {
		return xmlrpcValue{}, errors.New("rTorrent 请求上下文无效")
	}
	if err := ctx.Err(); err != nil {
		return xmlrpcValue{}, err
	}
	if client == nil || strings.TrimSpace(endpoint) == "" {
		return xmlrpcValue{}, errors.New("rTorrent 客户端未初始化")
	}
	var body strings.Builder
	body.WriteString(`<?xml version="1.0"?><methodCall><methodName>`)
	body.WriteString(escapeXML(method))
	body.WriteString(`</methodName><params>`)
	for _, param := range params {
		body.WriteString("<param>")
		body.WriteString(xmlrpcString(param))
		body.WriteString("</param>")
	}
	body.WriteString(`</params></methodCall>`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(body.String()))
	if err != nil {
		return xmlrpcValue{}, errors.New("rTorrent 请求无效")
	}
	request.Header.Set("Content-Type", "text/xml")
	if username != "" || password != "" {
		request.SetBasicAuth(username, password)
	}
	response, err := client.Do(request)
	if err != nil {
		return xmlrpcValue{}, errors.New("rTorrent 请求失败")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return xmlrpcValue{}, errors.New("rTorrent 请求失败")
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, xmlrpcMaxBody))
	if err != nil {
		return xmlrpcValue{}, errors.New("rTorrent 响应读取失败")
	}
	return decodeXMLRPCResponse(payload)
}

func decodeXMLRPCResponse(payload []byte) (xmlrpcValue, error) {
	if bytes.Contains(payload, []byte("<fault>")) {
		return xmlrpcValue{}, errors.New("rTorrent 返回错误")
	}
	var envelope struct {
		Params []xmlrpcValue `xml:"params>param>value"`
	}
	if err := xml.Unmarshal(payload, &envelope); err != nil {
		return xmlrpcValue{}, errors.New("rTorrent 响应解析失败")
	}
	if len(envelope.Params) == 0 {
		return xmlrpcValue{}, nil
	}
	return envelope.Params[0], nil
}

// xmlrpcEncodeBase64 是 rTorrent `d.load.raw` 需要的 base64 载荷。
func xmlrpcEncodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
