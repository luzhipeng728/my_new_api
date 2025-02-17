package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"one-api/common"
	"one-api/dto"
	"one-api/middleware"
	"one-api/model"
	"one-api/relay"
	"one-api/relay/constant"
	relayconstant "one-api/relay/constant"
	"one-api/service"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func relayHandler(c *gin.Context, relayMode int) *dto.OpenAIErrorWithStatusCode {
	var err *dto.OpenAIErrorWithStatusCode
	switch relayMode {
	case relayconstant.RelayModeImagesGenerations:
		err = relay.RelayImageHelper(c, relayMode)
	case relayconstant.RelayModeAudioSpeech:
		fallthrough
	case relayconstant.RelayModeAudioTranslation:
		fallthrough
	case relayconstant.RelayModeAudioTranscription:
		err = relay.AudioHelper(c, relayMode)
	default:
		err = relay.TextHelper(c)
	}
	return err
}

func Relay(c *gin.Context) {
	startTime := time.Now()
	relayMode := constant.Path2RelayMode(c.Request.URL.Path)
	retryTimes := common.RetryTimes
	requestId := c.GetString(common.RequestIdKey)
	channelId := c.GetInt("channel_id")
	channelType := c.GetInt("channel_type")
	group := c.GetString("group")
	originalModel := c.GetString("original_model")
	openaiErr := relayHandler(c, relayMode)

	input_tokens := 0
	textRequest := &dto.GeneralOpenAIRequest{}
	err := common.UnmarshalBodyReusable(c, textRequest)
	if err == nil {
		prompt_tokens, err := service.CountTokenChatRequest(*textRequest, textRequest.Model)
		if err == nil {
			input_tokens = prompt_tokens
		}
	}
	fmt.Println("input_tokens: ", input_tokens)

	c.Set("use_channel", []string{fmt.Sprintf("%d", channelId)})
	if openaiErr != nil {
		go processChannelError(c, channelId, channelType, openaiErr)
	} else {
		retryTimes = 0
	}
	for i := 0; shouldRetry(c, channelId, openaiErr, retryTimes) && i < retryTimes; i++ {
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
				// 打印下 logprobs 的值
				// fmt.Println("logprobs: ", logprobs)
				if logprobs == true {
					isNORLogprobs = true
				}
			}
			// 如果 包含 n 字段，如果不是 1， isNORLogprobs 为 true
			if n, exists := requestData["n"]; exists {
				// 转换成int类型,转换失败就是1
				tmp_n := 1
				if n_int, ok := n.(int); ok {
					tmp_n = n_int
				}

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
		channel, err := model.CacheGetRandomSatisfiedChannel(group, originalModel, i, isImage, isStream, isSystemPrompt, isNORLogprobs, isFunctionCall, input_tokens)
		if err != nil {
			common.LogError(c.Request.Context(), fmt.Sprintf("CacheGetRandomSatisfiedChannel failed: %s", err.Error()))
			break
		}
		fmt.Println("1: ")
		channelId = channel.Id
		useChannel := c.GetStringSlice("use_channel")
		useChannel = append(useChannel, fmt.Sprintf("%d", channelId))
		c.Set("use_channel", useChannel)
		common.LogInfo(c.Request.Context(), fmt.Sprintf("using channel #%d to retry (remain times %d)", channel.Id, i))
		middleware.SetupContextForSelectedChannel(c, channel, originalModel)

		requestBody, _ := common.GetRequestBody(c)
		c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
		openaiErr = relayHandler(c, relayMode)
		if openaiErr != nil {
			go processChannelError(c, channelId, channel.Type, openaiErr)
		}
	}
	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		common.LogInfo(c.Request.Context(), retryLogStr)
	}

	// Custom log info similar to [GIN] log
	// clientIP := c.ClientIP()
	latency := time.Since(startTime) // 计算响应延迟时间
	statusCode := c.Writer.Status()
	// method := c.Request.Method
	// path := c.Request.URL.Path
	retries := common.RetryTimes - retryTimes

	common.DebugLog(fmt.Sprintf("%3d | %13v| Model: %s | Retries: %d", statusCode, latency, originalModel, retries))

	if openaiErr != nil {
		if openaiErr.StatusCode == http.StatusTooManyRequests {
			openaiErr.Error.Message = "当前分组上游负载已饱和，请稍后再试"
		}
		openaiErr.Error.Message = common.MessageWithRequestId(openaiErr.Error.Message, requestId)
		c.JSON(openaiErr.StatusCode, gin.H{
			"error": openaiErr.Error,
		})
	}
}

func shouldRetry(c *gin.Context, channelId int, openaiErr *dto.OpenAIErrorWithStatusCode, retryTimes int) bool {
	if openaiErr == nil {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	if openaiErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if openaiErr.StatusCode == 307 {
		return true
	}
	if openaiErr.StatusCode/100 == 5 {
		// 超时不重试
		if openaiErr.StatusCode == 504 || openaiErr.StatusCode == 524 {
			return false
		}
		return true
	}
	if openaiErr.StatusCode == http.StatusBadRequest {
		return false
	}
	if openaiErr.StatusCode == 408 {
		// azure处理超时不重试
		return false
	}
	if openaiErr.LocalError {
		return false
	}
	if openaiErr.StatusCode/100 == 2 {
		return false
	}
	return true
}

func processChannelError(c *gin.Context, channelId int, channelType int, err *dto.OpenAIErrorWithStatusCode) {
	autoBan := c.GetBool("auto_ban")
	common.LogError(c.Request.Context(), fmt.Sprintf("relay error (channel #%d, status code: %d): %s", channelId, err.StatusCode, err.Error.Message))
	if service.ShouldDisableChannel(channelType, err) && autoBan {
		channelName := c.GetString("channel_name")
		service.DisableChannel(channelId, channelName, err.Error.Message)
	}
}

func RelayMidjourney(c *gin.Context) {
	relayMode := c.GetInt("relay_mode")
	var err *dto.MidjourneyResponse
	switch relayMode {
	case relayconstant.RelayModeMidjourneyNotify:
		err = relay.RelayMidjourneyNotify(c)
	case relayconstant.RelayModeMidjourneyTaskFetch, relayconstant.RelayModeMidjourneyTaskFetchByCondition:
		err = relay.RelayMidjourneyTask(c, relayMode)
	case relayconstant.RelayModeMidjourneyTaskImageSeed:
		err = relay.RelayMidjourneyTaskImageSeed(c)
	case relayconstant.RelayModeSwapFace:
		err = relay.RelaySwapFace(c)
	default:
		err = relay.RelayMidjourneySubmit(c, relayMode)
	}
	//err = relayMidjourneySubmit(c, relayMode)
	log.Println(err)
	if err != nil {
		statusCode := http.StatusBadRequest
		if err.Code == 30 {
			err.Result = "当前分组负载已饱和，请稍后再试，或升级账户以提升服务质量。"
			statusCode = http.StatusTooManyRequests
		}
		c.JSON(statusCode, gin.H{
			"description": fmt.Sprintf("%s %s", err.Description, err.Result),
			"type":        "upstream_error",
			"code":        err.Code,
		})
		channelId := c.GetInt("channel_id")
		common.LogError(c, fmt.Sprintf("relay error (channel #%d, status code %d): %s", channelId, statusCode, fmt.Sprintf("%s %s", err.Description, err.Result)))
	}
}

func RelayNotImplemented(c *gin.Context) {
	err := dto.OpenAIError{
		Message: "API not implemented",
		Type:    "new_api_error",
		Param:   "",
		Code:    "api_not_implemented",
	}
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": err,
	})
}

func RelayNotFound(c *gin.Context) {
	err := dto.OpenAIError{
		Message: fmt.Sprintf("Invalid URL (%s %s)", c.Request.Method, c.Request.URL.Path),
		Type:    "invalid_request_error",
		Param:   "",
		Code:    "",
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": err,
	})
}

func RelayTask(c *gin.Context) {
	retryTimes := common.RetryTimes
	channelId := c.GetInt("channel_id")
	relayMode := c.GetInt("relay_mode")
	group := c.GetString("group")
	originalModel := c.GetString("original_model")
	c.Set("use_channel", []string{fmt.Sprintf("%d", channelId)})
	taskErr := taskRelayHandler(c, relayMode)
	if taskErr == nil {
		retryTimes = 0
	}

	input_tokens := 0
	textRequest := &dto.GeneralOpenAIRequest{}
	err := common.UnmarshalBodyReusable(c, textRequest)
	if err == nil {
		prompt_tokens, err := service.CountTokenChatRequest(*textRequest, textRequest.Model)
		if err == nil {
			input_tokens = prompt_tokens
		}
	}
	fmt.Println("input_tokens: ", input_tokens)

	for i := 0; shouldRetryTaskRelay(c, channelId, taskErr, retryTimes) && i < retryTimes; i++ {
		var bodyBytes []byte
		if c.Request.Body != nil {
			bodyBytes, _ = io.ReadAll(c.Request.Body)
		}

		// 将 body 内容重新设置回 c.Request.Body
		c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
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
			// 如果 包含 logprobs 字段，查看是否是 true
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
				tmp_n := 1
				if n_int, ok := n.(int); ok {
					tmp_n = n_int
				}

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

		channel, err := model.CacheGetRandomSatisfiedChannel(group, originalModel, i, isImage, isStream, isSystemPrompt, isNORLogprobs, isFunctionCall, input_tokens)
		if err != nil {
			common.LogError(c.Request.Context(), fmt.Sprintf("CacheGetRandomSatisfiedChannel failed: %s", err.Error()))
			break
		}

		fmt.Println("2: ")
		channelId = channel.Id
		useChannel := c.GetStringSlice("use_channel")
		useChannel = append(useChannel, fmt.Sprintf("%d", channelId))
		c.Set("use_channel", useChannel)
		common.LogInfo(c.Request.Context(), fmt.Sprintf("using channel #%d to retry (remain times %d)", channel.Id, i))
		middleware.SetupContextForSelectedChannel(c, channel, originalModel)

		requestBody, _ := common.GetRequestBody(c)
		c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
		taskErr = taskRelayHandler(c, relayMode)
	}
	useChannel := c.GetStringSlice("use_channel")
	if len(useChannel) > 1 {
		retryLogStr := fmt.Sprintf("重试：%s", strings.Trim(strings.Join(strings.Fields(fmt.Sprint(useChannel)), "->"), "[]"))
		common.LogInfo(c.Request.Context(), retryLogStr)
	}
	if taskErr != nil {
		if taskErr.StatusCode == http.StatusTooManyRequests {
			taskErr.Message = "当前分组上游负载已饱和，请稍后再试"
		}
		c.JSON(taskErr.StatusCode, taskErr)
	}
}

func taskRelayHandler(c *gin.Context, relayMode int) *dto.TaskError {
	var err *dto.TaskError
	switch relayMode {
	case relayconstant.RelayModeSunoFetch, relayconstant.RelayModeSunoFetchByID:
		err = relay.RelayTaskFetch(c, relayMode)
	default:
		err = relay.RelayTaskSubmit(c, relayMode)
	}
	return err
}

func shouldRetryTaskRelay(c *gin.Context, channelId int, taskErr *dto.TaskError, retryTimes int) bool {
	if taskErr == nil {
		return false
	}
	if retryTimes <= 0 {
		return false
	}
	if _, ok := c.Get("specific_channel_id"); ok {
		return false
	}
	if taskErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if taskErr.StatusCode == 307 {
		return true
	}
	if taskErr.StatusCode/100 == 5 {
		// 超时不重试
		if taskErr.StatusCode == 504 || taskErr.StatusCode == 524 {
			return false
		}
		return true
	}
	if taskErr.StatusCode == http.StatusBadRequest {
		return false
	}
	if taskErr.StatusCode == 408 {
		// azure处理超时不重试
		return false
	}
	if taskErr.LocalError {
		return false
	}
	if taskErr.StatusCode/100 == 2 {
		return false
	}
	return true
}
