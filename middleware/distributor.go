package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"one-api/common"
	"one-api/constant"
	"one-api/dto"
	"one-api/model"
	relayconstant "one-api/relay/constant"
	"one-api/service"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type ModelRequest struct {
	Model string `json:"model"`
}

func geTexttRequest(c *gin.Context) (*dto.GeneralOpenAIRequest, error) {
	textRequest := &dto.GeneralOpenAIRequest{}
	err := common.UnmarshalBodyReusable(c, textRequest)
	if err != nil {
		return nil, err
	}
	if textRequest.Messages == nil || len(textRequest.Messages) == 0 {
		return nil, errors.New("field messages is required")
	}
	return textRequest, nil
}

func Distribute() func(c *gin.Context) {
	return func(c *gin.Context) {
		userId := c.GetInt("id")
		input_tokens := 0
		textRequest, err := geTexttRequest(c)
		if err != nil {
			fmt.Println(err)
		}
		prompt_tokens, err := service.CountTokenChatRequest(*textRequest, textRequest.Model)
		if err == nil {
			input_tokens = prompt_tokens
		}
		fmt.Println("input_tokens: ", input_tokens)

		var channel *model.Channel
		channelId, ok := c.Get("specific_channel_id")
		modelRequest, shouldSelectChannel, err := getModelRequest(c)
		if err != nil {
			fmt.Println(err)
		}
		userGroup, _ := model.CacheGetUserGroup(userId)
		c.Set("group", userGroup)
		if ok {
			id, err := strconv.Atoi(channelId.(string))
			if err != nil {
				abortWithOpenAiMessage(c, http.StatusBadRequest, "无效的渠道 Id")
				return
			}
			channel, err = model.GetChannelById(id, true)
			if err != nil {
				abortWithOpenAiMessage(c, http.StatusBadRequest, "无效的渠道 Id")
				return
			}
			if channel.Status != common.ChannelStatusEnabled {
				abortWithOpenAiMessage(c, http.StatusForbidden, "该渠道已被禁用")
				return
			}
		} else {
			// Select a channel for the user
			// check token model mapping
			modelLimitEnable := c.GetBool("token_model_limit_enabled")
			if modelLimitEnable {
				s, ok := c.Get("token_model_limit")
				var tokenModelLimit map[string]bool
				if ok {
					tokenModelLimit = s.(map[string]bool)
				} else {
					tokenModelLimit = map[string]bool{}
				}
				if tokenModelLimit != nil {
					if _, ok := tokenModelLimit[modelRequest.Model]; !ok {
						abortWithOpenAiMessage(c, http.StatusForbidden, "该令牌无权访问模型 "+modelRequest.Model)
						return
					}
				} else {
					// token model limit is empty, all models are not allowed
					abortWithOpenAiMessage(c, http.StatusForbidden, "该令牌无权访问任何模型")
					return
				}
			}
			// 在不影响后续通过c.Request.Body 获取 body 的情况下，将 body 读取出来

			// 读取 body
			var bodyBytes []byte
			if c.Request.Body != nil {
				bodyBytes, _ = io.ReadAll(c.Request.Body)
			}

			// 将 body 内容重新设置回 c.Request.Body
			c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

			// 解析 body 到请求的结构体

			// 解析 JSON 请求数据
			// 检查是否有 messages 字段并解析
			isImage := false
			isStream := false
			isSystemPrompt := false
			isNORLogprobs := false
			isFunctionCall := false
			var requestData map[string]interface{}
			if err := json.Unmarshal(bodyBytes, &requestData); err != nil {
				fmt.Println("解析 JSON 请求数据失败")
			} else {
				// 如果 包含 stream 字段，查看是否是 true
				if stream, exists := requestData["stream"]; exists {
					if stream == true {
						isStream = true
					}
				}
				// 如果 包含 logprobs 字段，查看是否是 NOR
				if logprobs, exists := requestData["logprobs"]; exists {
					// fmt.Println("logprobs: ", logprobs)
					// 打印下 logprobs 的值
					if logprobs == true {
						isNORLogprobs = true
					}
				}
				// 如果 包含 n 字段，如果不是 1， isNORLogprobs 为 true
				if n, exists := requestData["n"]; exists {
					// 转换成int类型,转换失败就是1
					tmp_n, err := strconv.Atoi(fmt.Sprintf("%v", n))
					if err != nil {
						tmp_n = 1
					}

					// fmt.Println("n: ", tmp_n)
					if tmp_n != 1 {
						isNORLogprobs = true
					}
				}
				// 如果 messages 是数组，遍历每个 message
				if messages, ok := requestData["messages"].([]interface{}); ok {
					for _, message := range messages {
						if msgMap, ok := message.(map[string]interface{}); ok {
							if content, exists := msgMap["content"]; exists {
								switch content.(type) {
								case []interface{}:
									for _, item := range content.([]interface{}) {
										if itemMap, ok := item.(map[string]interface{}); ok {
											if t, exists := itemMap["type"]; exists && t == "image_url" {
												isImage = true
											}
										}
									}
								}
							}
							// 如果tool_calls存在，说明是FunctionCall
							if _, exists := msgMap["tool_calls"]; exists {
								isFunctionCall = true
							}
							// 如果包含 role 字段，查看是否是 system
							if role, exists := msgMap["role"]; exists {
								if role == "system" {
									isSystemPrompt = true
								}
							}
						}
					}
				}
			}
			if shouldSelectChannel {
				channel, err = model.CacheGetRandomSatisfiedChannel(userGroup, modelRequest.Model, 0, isImage, isStream, isSystemPrompt, isNORLogprobs, isFunctionCall, input_tokens)

				fmt.Println("3: ")
				if err != nil {
					message := fmt.Sprintf("当前分组 %s 下对于模型 %s 无可用渠道", userGroup, modelRequest.Model)
					// 如果错误，但是渠道不为空，说明是数据库一致性问题
					if channel != nil {
						common.SysError(fmt.Sprintf("渠道不存在：%d", channel.Id))
						message = "数据库一致性已被破坏，请联系管理员"
					}
					// 如果错误，而且渠道为空，说明是没有可用渠道
					abortWithOpenAiMessage(c, http.StatusServiceUnavailable, message)
					return
				}
				if channel == nil {
					abortWithOpenAiMessage(c, http.StatusServiceUnavailable, fmt.Sprintf("当前分组 %s 下对于模型 %s 无可用渠道（数据库一致性已被破坏）", userGroup, modelRequest.Model))
					return
				}
			}
		}
		SetupContextForSelectedChannel(c, channel, modelRequest.Model)
		c.Next()
	}
}

func getModelRequest(c *gin.Context) (*ModelRequest, bool, error) {
	var modelRequest ModelRequest
	shouldSelectChannel := true
	var err error
	if strings.Contains(c.Request.URL.Path, "/mj/") {
		relayMode := relayconstant.Path2RelayModeMidjourney(c.Request.URL.Path)
		if relayMode == relayconstant.RelayModeMidjourneyTaskFetch ||
			relayMode == relayconstant.RelayModeMidjourneyTaskFetchByCondition ||
			relayMode == relayconstant.RelayModeMidjourneyNotify ||
			relayMode == relayconstant.RelayModeMidjourneyTaskImageSeed {
			shouldSelectChannel = false
		} else {
			midjourneyRequest := dto.MidjourneyRequest{}
			err = common.UnmarshalBodyReusable(c, &midjourneyRequest)
			if err != nil {
				abortWithMidjourneyMessage(c, http.StatusBadRequest, constant.MjErrorUnknown, "无效的请求, "+err.Error())
				return nil, false, err
			}
			midjourneyModel, mjErr, success := service.GetMjRequestModel(relayMode, &midjourneyRequest)
			if mjErr != nil {
				abortWithMidjourneyMessage(c, http.StatusBadRequest, mjErr.Code, mjErr.Description)
				return nil, false, fmt.Errorf(mjErr.Description)
			}
			if midjourneyModel == "" {
				if !success {
					abortWithMidjourneyMessage(c, http.StatusBadRequest, constant.MjErrorUnknown, "无效的请求, 无法解析模型")
					return nil, false, fmt.Errorf("无效的请求, 无法解析模型")
				} else {
					// task fetch, task fetch by condition, notify
					shouldSelectChannel = false
				}
			}
			modelRequest.Model = midjourneyModel
		}
		c.Set("relay_mode", relayMode)
	} else if strings.Contains(c.Request.URL.Path, "/suno/") {
		relayMode := relayconstant.Path2RelaySuno(c.Request.Method, c.Request.URL.Path)
		if relayMode == relayconstant.RelayModeSunoFetch ||
			relayMode == relayconstant.RelayModeSunoFetchByID {
			shouldSelectChannel = false
		} else {
			modelName := service.CoverTaskActionToModelName(constant.TaskPlatformSuno, c.Param("action"))
			modelRequest.Model = modelName
		}
		c.Set("platform", string(constant.TaskPlatformSuno))
		c.Set("relay_mode", relayMode)
	} else if !strings.HasPrefix(c.Request.URL.Path, "/v1/audio/transcriptions") {
		err = common.UnmarshalBodyReusable(c, &modelRequest)
	}
	if err != nil {
		abortWithOpenAiMessage(c, http.StatusBadRequest, "无效的请求, "+err.Error())
		return nil, false, err
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/moderations") {
		if modelRequest.Model == "" {
			modelRequest.Model = "text-moderation-stable"
		}
	}
	if strings.HasSuffix(c.Request.URL.Path, "embeddings") {
		if modelRequest.Model == "" {
			modelRequest.Model = c.Param("model")
		}
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/images/generations") {
		if modelRequest.Model == "" {
			modelRequest.Model = "dall-e"
		}
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/audio") {
		if modelRequest.Model == "" {
			if strings.HasPrefix(c.Request.URL.Path, "/v1/audio/speech") {
				modelRequest.Model = "tts-1"
			} else {
				modelRequest.Model = "whisper-1"
			}
		}
	}
	return &modelRequest, shouldSelectChannel, nil
}

func SetupContextForSelectedChannel(c *gin.Context, channel *model.Channel, modelName string) {
	c.Set("original_model", modelName) // for retry
	if channel == nil {
		return
	}
	c.Set("channel", channel.Type)
	c.Set("channel_id", channel.Id)
	c.Set("channel_name", channel.Name)
	c.Set("channel_type", channel.Type)
	ban := true
	// parse *int to bool
	if channel.AutoBan != nil && *channel.AutoBan == 0 {
		ban = false
	}
	if nil != channel.OpenAIOrganization && "" != *channel.OpenAIOrganization {
		c.Set("channel_organization", *channel.OpenAIOrganization)
	}
	c.Set("auto_ban", ban)
	c.Set("model_mapping", channel.GetModelMapping())
	c.Set("status_code_mapping", channel.GetStatusCodeMapping())
	c.Request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", channel.Key))
	c.Set("base_url", channel.GetBaseURL())
	// TODO: api_version统一
	switch channel.Type {
	case common.ChannelTypeAzure:
		c.Set("api_version", channel.Other)
	case common.ChannelTypeXunfei:
		c.Set("api_version", channel.Other)
	//case common.ChannelTypeAIProxyLibrary:
	//	c.Set("library_id", channel.Other)
	case common.ChannelTypeGemini:
		c.Set("api_version", channel.Other)
	case common.ChannelTypeAli:
		c.Set("plugin", channel.Other)
	}
}
