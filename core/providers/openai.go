// Package providers implements various LLM providers and their utility functions.
// This file contains the OpenAI provider implementation.
package providers

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	schemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// // openAIResponsePool provides a pool for OpenAI response objects.
// var openAIResponsePool = sync.Pool{
// 	New: func() interface{} {
// 		return &schemas.BifrostResponse{}
// 	},
// }

// // acquireOpenAIResponse gets an OpenAI response from the pool and resets it.
// func acquireOpenAIResponse() *schemas.BifrostResponse {
// 	resp := openAIResponsePool.Get().(*schemas.BifrostResponse)
// 	*resp = schemas.BifrostResponse{} // Reset the struct
// 	return resp
// }

// // releaseOpenAIResponse returns an OpenAI response to the pool.
// func releaseOpenAIResponse(resp *schemas.BifrostResponse) {
// 	if resp != nil {
// 		openAIResponsePool.Put(resp)
// 	}
// }

// OpenAIProvider implements the Provider interface for OpenAI's GPT API.
type OpenAIProvider struct {
	logger               schemas.Logger                // Logger for provider operations
	client               *fasthttp.Client              // HTTP client for API requests
	streamClient         *http.Client                  // HTTP client for streaming requests
	networkConfig        schemas.NetworkConfig         // Network configuration including extra headers
	sendBackRawResponse  bool                          // Whether to include raw response in BifrostResponse
	customProviderConfig *schemas.CustomProviderConfig // Custom provider config
}

// NewOpenAIProvider creates a new OpenAI provider instance.
// It initializes the HTTP client with the provided configuration and sets up response pools.
// The client is configured with timeouts, concurrency limits, and optional proxy settings.
func NewOpenAIProvider(config *schemas.ProviderConfig, logger schemas.Logger) *OpenAIProvider {
	config.CheckAndSetDefaults()

	client := &fasthttp.Client{
		ReadTimeout:     time.Second * time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds),
		WriteTimeout:    time.Second * time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds),
		MaxConnsPerHost: config.ConcurrencyAndBufferSize.Concurrency,
	}

	// Initialize streaming HTTP client
	streamClient := &http.Client{
		Timeout: time.Second * time.Duration(config.NetworkConfig.DefaultRequestTimeoutInSeconds),
	}

	// // Pre-warm response pools
	// for range config.ConcurrencyAndBufferSize.Concurrency {
	// 	openAIResponsePool.Put(&schemas.BifrostResponse{})
	// }

	// Configure proxy if provided
	client = configureProxy(client, config.ProxyConfig, logger)

	// Set default BaseURL if not provided
	if config.NetworkConfig.BaseURL == "" {
		config.NetworkConfig.BaseURL = "https://api.openai.com"
	}
	config.NetworkConfig.BaseURL = strings.TrimRight(config.NetworkConfig.BaseURL, "/")

	return &OpenAIProvider{
		logger:               logger,
		client:               client,
		streamClient:         streamClient,
		networkConfig:        config.NetworkConfig,
		sendBackRawResponse:  config.SendBackRawResponse,
		customProviderConfig: config.CustomProviderConfig,
	}
}

// GetProviderKey returns the provider identifier for OpenAI.
func (provider *OpenAIProvider) GetProviderKey() schemas.ModelProvider {
	return getProviderName(schemas.OpenAI, provider.customProviderConfig)
}

// TextCompletion is not supported by the OpenAI provider.
// Returns an error indicating that text completion is not available.
func (provider *OpenAIProvider) TextCompletion(ctx context.Context, model string, key schemas.Key, text string, params *schemas.ModelParameters) (*schemas.BifrostResponse, *schemas.BifrostError) {
	return nil, newUnsupportedOperationError("text completion", "openai")
}

// ChatCompletion performs a chat completion request to the OpenAI API.
// It supports both text and image content in messages.
// Returns a BifrostResponse containing the completion results or an error if the request fails.
func (provider *OpenAIProvider) ChatCompletion(ctx context.Context, model string, key schemas.Key, messages []schemas.BifrostMessage, params *schemas.ModelParameters) (*schemas.BifrostResponse, *schemas.BifrostError) {
	// Check if chat completion is allowed for this provider
	if err := checkOperationAllowed(schemas.OpenAI, provider.customProviderConfig, schemas.OperationChatCompletion); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()

	formattedMessages, preparedParams := prepareOpenAIChatRequest(messages, params)

	requestBody := mergeConfig(map[string]interface{}{
		"model":    model,
		"messages": formattedMessages,
	}, preparedParams)

	jsonBody, err := sonic.Marshal(requestBody)
	if err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderJSONMarshaling, err, providerName)
	}

	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Set any extra headers from network config
	setExtraHeaders(req, provider.networkConfig.ExtraHeaders, nil)

	req.SetRequestURI(provider.networkConfig.BaseURL + "/v1/chat/completions")
	req.Header.SetMethod("POST")
	req.Header.SetContentType("application/json")
	req.Header.Set("Authorization", "Bearer "+key.Value)

	req.SetBody(jsonBody)

	// Make request
	bifrostErr := makeRequestWithContext(ctx, provider.client, req, resp)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		provider.logger.Debug(fmt.Sprintf("error from %s provider: %s", providerName, string(resp.Body())))
		return nil, parseOpenAIError(resp)
	}

	responseBody := resp.Body()

	// Parse and preprocess reasoning fields
	var rawMap map[string]interface{}
	if err := sonic.Unmarshal(responseBody, &rawMap); err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderResponseUnmarshal, err, providerName)
	}

	// Map reasoning_content/reasoning to thought in message
	if choices, ok := rawMap["choices"].([]interface{}); ok {
		for _, choice := range choices {
			if choiceMap, ok := choice.(map[string]interface{}); ok {
				if message, ok := choiceMap["message"].(map[string]interface{}); ok {
					if rc, exists := message["reasoning_content"]; exists {
						message["thought"] = rc
						delete(message, "reasoning_content")
					} else if r, exists := message["reasoning"]; exists {
						message["thought"] = r
						delete(message, "reasoning")
					}
				}
			}
		}
	}

	// Re-marshal and parse into BifrostResponse
	modifiedBody, err := sonic.Marshal(rawMap)
	if err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderJSONMarshaling, err, providerName)
	}

	response := &schemas.BifrostResponse{}
	if err := sonic.Unmarshal(modifiedBody, response); err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderResponseUnmarshal, err, providerName)
	}

	response.ExtraFields.Provider = providerName

	if provider.sendBackRawResponse {
		response.ExtraFields.RawResponse = rawMap
	}

	if params != nil {
		response.ExtraFields.Params = *params
	}

	return response, nil
}

// prepareOpenAIChatRequest formats messages for the OpenAI API.
// It handles both text and image content in messages.
// Returns a slice of formatted messages and any additional parameters.
func prepareOpenAIChatRequest(messages []schemas.BifrostMessage, params *schemas.ModelParameters) ([]map[string]interface{}, map[string]interface{}) {
	// Format messages for OpenAI API
	var formattedMessages []map[string]interface{}
	for _, msg := range messages {
		if msg.Role == schemas.ModelChatMessageRoleAssistant {
			assistantMessage := map[string]interface{}{
				"role":    msg.Role,
				"content": msg.Content,
			}
			if msg.AssistantMessage != nil && msg.AssistantMessage.ToolCalls != nil {
				assistantMessage["tool_calls"] = *msg.AssistantMessage.ToolCalls
			}
			formattedMessages = append(formattedMessages, assistantMessage)
		} else {
			message := map[string]interface{}{
				"role": msg.Role,
			}

			if msg.Content.ContentStr != nil {
				message["content"] = *msg.Content.ContentStr
			} else if msg.Content.ContentBlocks != nil {
				contentBlocks := *msg.Content.ContentBlocks
				for i := range contentBlocks {
					if contentBlocks[i].Type == schemas.ContentBlockTypeImage && contentBlocks[i].ImageURL != nil {
						sanitizedURL, _ := SanitizeImageURL(contentBlocks[i].ImageURL.URL)
						contentBlocks[i].ImageURL.URL = sanitizedURL
					}
				}

				message["content"] = contentBlocks
			}

			if msg.ToolMessage != nil && msg.ToolMessage.ToolCallID != nil {
				message["tool_call_id"] = *msg.ToolMessage.ToolCallID
			}

			formattedMessages = append(formattedMessages, message)
		}
	}

	preparedParams := prepareParams(params)

	return formattedMessages, preparedParams
}

// Embedding generates embeddings for the given input text(s).
// The input can be either a single string or a slice of strings for batch embedding.
// Returns a BifrostResponse containing the embedding(s) and any error that occurred.
func (provider *OpenAIProvider) Embedding(ctx context.Context, model string, key schemas.Key, input *schemas.EmbeddingInput, params *schemas.ModelParameters) (*schemas.BifrostResponse, *schemas.BifrostError) {
	// Check if embedding is allowed for this provider
	if err := checkOperationAllowed(schemas.OpenAI, provider.customProviderConfig, schemas.OperationEmbedding); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()

	requestBody := prepareOpenAIEmbeddingRequest(input, params)
	requestBody["model"] = model

	// Use the shared embedding request handler
	return handleOpenAIEmbeddingRequest(
		ctx,
		provider.client,
		provider.networkConfig.BaseURL+"/v1/embeddings",
		requestBody,
		key,
		params,
		provider.networkConfig.ExtraHeaders,
		providerName,
		provider.sendBackRawResponse,
		provider.logger,
	)
}

func prepareOpenAIEmbeddingRequest(input *schemas.EmbeddingInput, params *schemas.ModelParameters) map[string]interface{} {
	requestBody := map[string]interface{}{
		"input": input,
	}

	// Merge any additional parameters
	if params != nil {
		// Map standard parameters
		if params.EncodingFormat != nil {
			requestBody["encoding_format"] = *params.EncodingFormat
		}
		if params.Dimensions != nil {
			requestBody["dimensions"] = *params.Dimensions
		}
		if params.User != nil {
			requestBody["user"] = *params.User
		}

		// Merge any extra parameters
		requestBody = mergeConfig(requestBody, params.ExtraParams)
	}

	return requestBody
}

func handleOpenAIEmbeddingRequest(ctx context.Context, client *fasthttp.Client, url string, requestBody map[string]interface{}, key schemas.Key, params *schemas.ModelParameters, extraHeaders map[string]string, providerName schemas.ModelProvider, sendBackRawResponse bool, logger schemas.Logger) (*schemas.BifrostResponse, *schemas.BifrostError) {
	jsonBody, err := sonic.Marshal(requestBody)
	if err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderJSONMarshaling, err, providerName)
	}

	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Set any extra headers from network config
	setExtraHeaders(req, extraHeaders, nil)

	req.SetRequestURI(url)
	req.Header.SetMethod("POST")
	req.Header.SetContentType("application/json")
	req.Header.Set("Authorization", "Bearer "+key.Value)

	req.SetBody(jsonBody)

	// Make request
	bifrostErr := makeRequestWithContext(ctx, client, req, resp)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		logger.Debug(fmt.Sprintf("error from %s provider: %s", providerName, string(resp.Body())))
		return nil, parseOpenAIError(resp)
	}

	responseBody := resp.Body()

	// Pre-allocate response structs
	response := &schemas.BifrostResponse{}

	// Use enhanced response handler with pre-allocated response
	rawResponse, bifrostErr := handleProviderResponse(responseBody, response, sendBackRawResponse)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	response.ExtraFields.Provider = providerName

	if params != nil {
		response.ExtraFields.Params = *params
	}

	if sendBackRawResponse {
		response.ExtraFields.RawResponse = rawResponse
	}

	return response, nil
}

// ChatCompletionStream handles streaming for OpenAI chat completions.
// It formats messages, prepares request body, and uses shared streaming logic.
// Returns a channel for streaming responses and any error that occurred.
func (provider *OpenAIProvider) ChatCompletionStream(ctx context.Context, postHookRunner schemas.PostHookRunner, model string, key schemas.Key, messages []schemas.BifrostMessage, params *schemas.ModelParameters) (chan *schemas.BifrostStream, *schemas.BifrostError) {
	// Check if chat completion stream is allowed for this provider
	if err := checkOperationAllowed(schemas.OpenAI, provider.customProviderConfig, schemas.OperationChatCompletionStream); err != nil {
		return nil, err
	}

	formattedMessages, preparedParams := prepareOpenAIChatRequest(messages, params)

	requestBody := mergeConfig(map[string]interface{}{
		"model":    model,
		"messages": formattedMessages,
		"stream":   true,
		"stream_options": map[string]interface{}{
			"include_usage": true,
		},
	}, preparedParams)

	// Prepare OpenAI headers
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + key.Value,
		"Accept":        "text/event-stream",
		"Cache-Control": "no-cache",
	}

	providerName := provider.GetProviderKey()

	// Use shared streaming logic
	return handleOpenAIStreaming(
		ctx,
		provider.streamClient,
		provider.networkConfig.BaseURL+"/v1/chat/completions",
		requestBody,
		headers,
		provider.networkConfig.ExtraHeaders,
		providerName,
		params,
		postHookRunner,
		provider.logger,
	)
}

// performOpenAICompatibleStreaming handles streaming for OpenAI-compatible APIs (OpenAI, Azure).
// This shared function reduces code duplication between providers that use the same SSE format.
func handleOpenAIStreaming(
	ctx context.Context,
	httpClient *http.Client,
	url string,
	requestBody map[string]interface{},
	headers map[string]string,
	extraHeaders map[string]string,
	providerName schemas.ModelProvider,
	params *schemas.ModelParameters,
	postHookRunner schemas.PostHookRunner,
	logger schemas.Logger,
) (chan *schemas.BifrostStream, *schemas.BifrostError) {

	jsonBody, err := sonic.Marshal(requestBody)
	if err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderJSONMarshaling, err, providerName)
	}

	// Create HTTP request for streaming
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderRequest, err, providerName)
	}

	// Set any extra headers from network config
	setExtraHeadersHTTP(req, extraHeaders, nil)

	// Set headers
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	// Make the request
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderRequest, err, providerName)
	}

	// Check for HTTP errors
	if resp.StatusCode != http.StatusOK {
		return nil, parseStreamOpenAIError(resp)
	}

	// Create response channel
	responseChan := make(chan *schemas.BifrostStream, schemas.DefaultStreamBufferSize)

	// Start streaming in a goroutine
	go func() {
		defer close(responseChan)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		chunkIndex := -1
		usage := &schemas.LLMUsage{}

		var finishReason *string
		var id string

		for scanner.Scan() {
			line := scanner.Text()

			// Skip empty lines and comments
			if line == "" || strings.HasPrefix(line, ":") {
				continue
			}

			// Check for end of stream
			if line == "data: [DONE]" {
				break
			}

			var jsonData string

			// Parse SSE data
			if strings.HasPrefix(line, "data: ") {
				jsonData = strings.TrimPrefix(line, "data: ")
			} else {
				// Handle raw JSON errors (without "data: " prefix)
				jsonData = line
			}

			// Skip empty data
			if strings.TrimSpace(jsonData) == "" {
				continue
			}

			// Parse as raw map to check for errors and preprocess reasoning fields
			var rawChunk map[string]interface{}
			if err := sonic.Unmarshal([]byte(jsonData), &rawChunk); err != nil {
				logger.Warn(fmt.Sprintf("Failed to parse stream data as JSON: %v", err))
				continue
			}

			// Handle error responses
			if _, hasError := rawChunk["error"]; hasError {
				bifrostErr, err := parseOpenAIErrorForStreamDataLine(jsonData)
				if err != nil {
					logger.Warn(fmt.Sprintf("Failed to parse error response: %v", err))
					continue
				}
				ctx = context.WithValue(ctx, schemas.BifrostContextKeyStreamEndIndicator, true)
				processAndSendBifrostError(ctx, postHookRunner, bifrostErr, responseChan, logger)
				return
			}

			// Map reasoning_content/reasoning to thought in delta for reasoning models
			if choices, ok := rawChunk["choices"].([]interface{}); ok {
				for _, choice := range choices {
					if choiceMap, ok := choice.(map[string]interface{}); ok {
						if delta, ok := choiceMap["delta"].(map[string]interface{}); ok {
							if rc, exists := delta["reasoning_content"]; exists {
								delta["thought"] = rc
								delete(delta, "reasoning_content")
							} else if r, exists := delta["reasoning"]; exists {
								delta["thought"] = r
								delete(delta, "reasoning")
							}
						}
					}
				}
				// Re-marshal the modified data
				if modifiedJSON, err := sonic.Marshal(rawChunk); err == nil {
					jsonData = string(modifiedJSON)
				}
			}

			// Parse into bifrost response
			var response schemas.BifrostResponse
			if err := sonic.Unmarshal([]byte(jsonData), &response); err != nil {
				logger.Warn(fmt.Sprintf("Failed to parse stream response: %v", err))
				continue
			}

			// Handle usage-only chunks (when stream_options include_usage is true)
			if response.Usage != nil {
				// Collect usage information and send at the end of the stream
				// Here in some cases usage comes before final message
				// So we need to check if the response.Usage is nil and then if usage != nil
				// then add up all tokens
				if response.Usage.PromptTokens > usage.PromptTokens {
					usage.PromptTokens = response.Usage.PromptTokens
				}
				if response.Usage.CompletionTokens > usage.CompletionTokens {
					usage.CompletionTokens = response.Usage.CompletionTokens
				}
				if response.Usage.TotalTokens > usage.TotalTokens {
					usage.TotalTokens = response.Usage.TotalTokens
				}
				calculatedTotal := usage.PromptTokens + usage.CompletionTokens
				if calculatedTotal > usage.TotalTokens {
					usage.TotalTokens = calculatedTotal
				}
				response.Usage = nil
			}

			// Skip empty responses or responses without choices
			if len(response.Choices) == 0 {
				continue
			}

			// Handle finish reason, usually in the final chunk
			choice := response.Choices[0]
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				// Collect finish reason and send at the end of the stream
				finishReason = choice.FinishReason
				response.Choices[0].FinishReason = nil
			}

			if response.ID != "" && id == "" {
				id = response.ID
			}

			// Handle regular content chunks
			if choice.BifrostStreamResponseChoice != nil && (choice.BifrostStreamResponseChoice.Delta.Content != nil || len(choice.BifrostStreamResponseChoice.Delta.ToolCalls) > 0) {
				chunkIndex++

				response.ExtraFields.Provider = providerName
				response.ExtraFields.ChunkIndex = chunkIndex

				processAndSendResponse(ctx, postHookRunner, &response, responseChan, logger)
			}
		}

		// Handle scanner errors first
		if err := scanner.Err(); err != nil {
			logger.Warn(fmt.Sprintf("Error reading stream: %v", err))
			processAndSendError(ctx, postHookRunner, err, responseChan, logger)
		} else {
			response := createBifrostChatCompletionChunkResponse(id, usage, finishReason, chunkIndex, params, providerName)
			handleStreamEndWithSuccess(ctx, response, postHookRunner, responseChan, logger)
		}
	}()

	return responseChan, nil
}

// Speech handles non-streaming speech synthesis requests.
// It formats the request body, makes the API call, and returns the response.
// Returns the response and any error that occurred.
func (provider *OpenAIProvider) Speech(ctx context.Context, model string, key schemas.Key, input *schemas.SpeechInput, params *schemas.ModelParameters) (*schemas.BifrostResponse, *schemas.BifrostError) {
	if err := checkOperationAllowed(schemas.OpenAI, provider.customProviderConfig, schemas.OperationSpeech); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()

	responseFormat := input.ResponseFormat
	if responseFormat == "" {
		responseFormat = "mp3"
	}

	requestBody := map[string]interface{}{
		"input":           input.Input,
		"model":           model,
		"voice":           input.VoiceConfig.Voice,
		"instructions":    input.Instructions,
		"response_format": responseFormat,
	}

	if params != nil {
		requestBody = mergeConfig(requestBody, params.ExtraParams)
	}

	jsonBody, err := sonic.Marshal(requestBody)
	if err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderJSONMarshaling, err, providerName)
	}

	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Set any extra headers from network config
	setExtraHeaders(req, provider.networkConfig.ExtraHeaders, nil)

	req.SetRequestURI(provider.networkConfig.BaseURL + "/v1/audio/speech")
	req.Header.SetMethod("POST")
	req.Header.SetContentType("application/json")
	req.Header.Set("Authorization", "Bearer "+key.Value)

	req.SetBody(jsonBody)

	// Make request
	bifrostErr := makeRequestWithContext(ctx, provider.client, req, resp)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		provider.logger.Debug(fmt.Sprintf("error from %s provider: %s", providerName, string(resp.Body())))
		return nil, parseOpenAIError(resp)
	}

	// Get the binary audio data from the response body
	audioData := resp.Body()

	// Create final response with the audio data
	// Note: For speech synthesis, we return the binary audio data in the raw response
	// The audio data is typically in MP3, WAV, or other audio formats as specified by response_format
	bifrostResponse := &schemas.BifrostResponse{
		Object: "audio.speech",
		Model:  model,
		Speech: &schemas.BifrostSpeech{
			Audio: audioData,
		},
		ExtraFields: schemas.BifrostResponseExtraFields{
			Provider: providerName,
		},
	}

	if params != nil {
		bifrostResponse.ExtraFields.Params = *params
	}

	return bifrostResponse, nil
}

// SpeechStream handles streaming for speech synthesis.
// It formats the request body, creates HTTP request, and uses shared streaming logic.
// Returns a channel for streaming responses and any error that occurred.
func (provider *OpenAIProvider) SpeechStream(ctx context.Context, postHookRunner schemas.PostHookRunner, model string, key schemas.Key, input *schemas.SpeechInput, params *schemas.ModelParameters) (chan *schemas.BifrostStream, *schemas.BifrostError) {
	if err := checkOperationAllowed(schemas.OpenAI, provider.customProviderConfig, schemas.OperationSpeechStream); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()

	responseFormat := input.ResponseFormat
	if responseFormat == "" {
		responseFormat = "mp3"
	}

	requestBody := map[string]interface{}{
		"input":           input.Input,
		"model":           model,
		"voice":           input.VoiceConfig.Voice,
		"instructions":    input.Instructions,
		"response_format": responseFormat,
		"stream_format":   "sse",
	}

	if params != nil {
		requestBody = mergeConfig(requestBody, params.ExtraParams)
	}

	jsonBody, err := sonic.Marshal(requestBody)
	if err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderJSONMarshaling, err, providerName)
	}

	// Prepare OpenAI headers
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + key.Value,
		"Accept":        "text/event-stream",
		"Cache-Control": "no-cache",
	}

	// Create HTTP request for streaming
	req, err := http.NewRequestWithContext(ctx, "POST", provider.networkConfig.BaseURL+"/v1/audio/speech", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderRequest, err, providerName)
	}

	// Set any extra headers from network config
	setExtraHeadersHTTP(req, provider.networkConfig.ExtraHeaders, nil)

	// Set headers
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	// Make the request
	resp, err := provider.streamClient.Do(req)
	if err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderRequest, err, providerName)
	}

	// Check for HTTP errors
	if resp.StatusCode != http.StatusOK {
		return nil, parseStreamOpenAIError(resp)
	}

	// Create response channel
	responseChan := make(chan *schemas.BifrostStream, schemas.DefaultStreamBufferSize)

	// Start streaming in a goroutine
	go func() {
		defer close(responseChan)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		chunkIndex := -1

		for scanner.Scan() {
			line := scanner.Text()

			// Skip empty lines and comments
			if line == "" || strings.HasPrefix(line, ":") {
				continue
			}

			// Check for end of stream
			if line == "data: [DONE]" {
				break
			}

			var jsonData string

			// Parse SSE data
			if strings.HasPrefix(line, "data: ") {
				jsonData = strings.TrimPrefix(line, "data: ")
			} else {
				// Handle raw JSON errors (without "data: " prefix)
				jsonData = line
			}

			// Skip empty data
			if strings.TrimSpace(jsonData) == "" {
				continue
			}

			// First, check if this is an error response
			var errorCheck map[string]interface{}
			if err := sonic.Unmarshal([]byte(jsonData), &errorCheck); err != nil {
				provider.logger.Warn(fmt.Sprintf("Failed to parse stream data as JSON: %v", err))
				continue
			}

			// Handle error responses
			if _, hasError := errorCheck["error"]; hasError {
				bifrostErr, err := parseOpenAIErrorForStreamDataLine(jsonData)
				if err != nil {
					provider.logger.Warn(fmt.Sprintf("Failed to parse error response: %v", err))
					continue
				}
				ctx = context.WithValue(ctx, schemas.BifrostContextKeyStreamEndIndicator, true)
				processAndSendBifrostError(ctx, postHookRunner, bifrostErr, responseChan, provider.logger)
				return
			}

			// Parse into bifrost response
			var response schemas.BifrostResponse

			var speechResponse schemas.BifrostSpeech
			if err := sonic.Unmarshal([]byte(jsonData), &speechResponse); err != nil {
				provider.logger.Warn(fmt.Sprintf("Failed to parse stream response: %v", err))
				continue
			}

			chunkIndex++

			response.Speech = &speechResponse
			response.Object = "audio.speech.chunk"
			response.Model = model
			response.ExtraFields = schemas.BifrostResponseExtraFields{
				Provider: providerName,
			}

			response.ExtraFields.ChunkIndex = chunkIndex

			if speechResponse.Usage != nil {
				if params != nil {
					response.ExtraFields.Params = *params
				}

				ctx = context.WithValue(ctx, schemas.BifrostContextKeyStreamEndIndicator, true)
				processAndSendResponse(ctx, postHookRunner, &response, responseChan, provider.logger)
				return
			}

			processAndSendResponse(ctx, postHookRunner, &response, responseChan, provider.logger)
		}

		// Handle scanner errors
		if err := scanner.Err(); err != nil {
			provider.logger.Warn(fmt.Sprintf("Error reading stream: %v", err))
			processAndSendError(ctx, postHookRunner, err, responseChan, provider.logger)
		}
	}()

	return responseChan, nil
}

// Transcription handles non-streaming transcription requests.
// It creates a multipart form, adds fields, makes the API call, and returns the response.
// Returns the response and any error that occurred.
func (provider *OpenAIProvider) Transcription(ctx context.Context, model string, key schemas.Key, input *schemas.TranscriptionInput, params *schemas.ModelParameters) (*schemas.BifrostResponse, *schemas.BifrostError) {
	if err := checkOperationAllowed(schemas.OpenAI, provider.customProviderConfig, schemas.OperationTranscription); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()

	// Create multipart form
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if bifrostErr := parseTranscriptionFormDataBody(writer, input, model, params, providerName); bifrostErr != nil {
		return nil, bifrostErr
	}

	// Create request
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	// Set any extra headers from network config
	setExtraHeaders(req, provider.networkConfig.ExtraHeaders, nil)

	req.SetRequestURI(provider.networkConfig.BaseURL + "/v1/audio/transcriptions")
	req.Header.SetMethod("POST")
	req.Header.SetContentType(writer.FormDataContentType()) // This sets multipart/form-data with boundary
	req.Header.Set("Authorization", "Bearer "+key.Value)

	req.SetBody(body.Bytes())

	// Make request
	bifrostErr := makeRequestWithContext(ctx, provider.client, req, resp)
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	// Handle error response
	if resp.StatusCode() != fasthttp.StatusOK {
		provider.logger.Debug(fmt.Sprintf("error from %s provider: %s", providerName, string(resp.Body())))
		return nil, parseOpenAIError(resp)
	}

	responseBody := resp.Body()

	// Parse OpenAI's transcription response directly into BifrostTranscribe
	transcribeResponse := &schemas.BifrostTranscribe{
		BifrostTranscribeNonStreamResponse: &schemas.BifrostTranscribeNonStreamResponse{},
	}

	if err := sonic.Unmarshal(responseBody, transcribeResponse); err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderResponseUnmarshal, err, providerName)
	}

	// Parse raw response for RawResponse field
	var rawResponse interface{}
	if err := sonic.Unmarshal(responseBody, &rawResponse); err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderDecodeRaw, err, providerName)
	}

	// Create final response
	bifrostResponse := &schemas.BifrostResponse{
		Object:     "audio.transcription",
		Model:      model,
		Transcribe: transcribeResponse,
		ExtraFields: schemas.BifrostResponseExtraFields{
			Provider: providerName,
		},
	}

	if provider.sendBackRawResponse {
		bifrostResponse.ExtraFields.RawResponse = rawResponse
	}

	if params != nil {
		bifrostResponse.ExtraFields.Params = *params
	}

	return bifrostResponse, nil

}

func (provider *OpenAIProvider) TranscriptionStream(ctx context.Context, postHookRunner schemas.PostHookRunner, model string, key schemas.Key, input *schemas.TranscriptionInput, params *schemas.ModelParameters) (chan *schemas.BifrostStream, *schemas.BifrostError) {
	if err := checkOperationAllowed(schemas.OpenAI, provider.customProviderConfig, schemas.OperationTranscriptionStream); err != nil {
		return nil, err
	}

	providerName := provider.GetProviderKey()

	// Create multipart form
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("stream", "true"); err != nil {
		return nil, newBifrostOperationError("failed to write stream field", err, providerName)
	}

	if bifrostErr := parseTranscriptionFormDataBody(writer, input, model, params, providerName); bifrostErr != nil {
		return nil, bifrostErr
	}

	// Prepare OpenAI headers
	headers := map[string]string{
		"Content-Type":  writer.FormDataContentType(),
		"Authorization": "Bearer " + key.Value,
		"Accept":        "text/event-stream",
		"Cache-Control": "no-cache",
	}

	// Create HTTP request for streaming
	req, err := http.NewRequestWithContext(ctx, "POST", provider.networkConfig.BaseURL+"/v1/audio/transcriptions", &body)
	if err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderRequest, err, providerName)
	}

	// Set any extra headers from network config
	setExtraHeadersHTTP(req, provider.networkConfig.ExtraHeaders, nil)

	// Set headers
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	// Make the request
	resp, err := provider.streamClient.Do(req)
	if err != nil {
		return nil, newBifrostOperationError(schemas.ErrProviderRequest, err, providerName)
	}

	// Check for HTTP errors
	if resp.StatusCode != http.StatusOK {
		return nil, parseStreamOpenAIError(resp)
	}

	// Create response channel
	responseChan := make(chan *schemas.BifrostStream, schemas.DefaultStreamBufferSize)

	// Start streaming in a goroutine
	go func() {
		defer close(responseChan)
		defer resp.Body.Close()

		scanner := bufio.NewScanner(resp.Body)
		chunkIndex := -1

		for scanner.Scan() {
			line := scanner.Text()

			// Skip empty lines and comments
			if line == "" {
				continue
			}

			// Check for end of stream
			if line == "data: [DONE]" {
				break
			}

			var jsonData string
			// Parse SSE data
			if strings.HasPrefix(line, "data: ") {
				jsonData = strings.TrimPrefix(line, "data: ")
			} else {
				// Handle raw JSON errors (without "data: " prefix)
				jsonData = line
			}

			// Skip empty data
			if strings.TrimSpace(jsonData) == "" {
				continue
			}

			// First, check if this is an error response
			var errorCheck map[string]interface{}
			if err := sonic.Unmarshal([]byte(jsonData), &errorCheck); err != nil {
				provider.logger.Warn(fmt.Sprintf("Failed to parse stream data as JSON: %v", err))
				continue
			}

			// Handle error responses
			if _, hasError := errorCheck["error"]; hasError {
				bifrostErr, err := parseOpenAIErrorForStreamDataLine(jsonData)
				if err != nil {
					provider.logger.Warn(fmt.Sprintf("Failed to parse error response: %v", err))
					continue
				}
				ctx = context.WithValue(ctx, schemas.BifrostContextKeyStreamEndIndicator, true)
				processAndSendBifrostError(ctx, postHookRunner, bifrostErr, responseChan, provider.logger)
				return
			}

			var response schemas.BifrostResponse

			var transcriptionResponse schemas.BifrostTranscribe
			if err := sonic.Unmarshal([]byte(jsonData), &transcriptionResponse); err != nil {
				provider.logger.Warn(fmt.Sprintf("Failed to parse stream response: %v", err))
				continue
			}

			chunkIndex++

			response.Transcribe = &transcriptionResponse
			response.Object = "audio.transcription.chunk"
			response.Model = model
			response.ExtraFields = schemas.BifrostResponseExtraFields{
				Provider: providerName,
			}

			response.ExtraFields.ChunkIndex = chunkIndex

			if transcriptionResponse.Usage != nil {
				if params != nil {
					response.ExtraFields.Params = *params
				}

				ctx = context.WithValue(ctx, schemas.BifrostContextKeyStreamEndIndicator, true)
				processAndSendResponse(ctx, postHookRunner, &response, responseChan, provider.logger)
				return
			}

			processAndSendResponse(ctx, postHookRunner, &response, responseChan, provider.logger)
		}

		// Handle scanner errors
		if err := scanner.Err(); err != nil {
			provider.logger.Warn(fmt.Sprintf("Error reading stream: %v", err))
			processAndSendError(ctx, postHookRunner, err, responseChan, provider.logger)
		}
	}()

	return responseChan, nil
}

func parseTranscriptionFormDataBody(writer *multipart.Writer, input *schemas.TranscriptionInput, model string, params *schemas.ModelParameters, providerName schemas.ModelProvider) *schemas.BifrostError {
	// Add file field
	fileWriter, err := writer.CreateFormFile("file", "audio.mp3") // OpenAI requires a filename
	if err != nil {
		return newBifrostOperationError("failed to create form file", err, providerName)
	}
	if _, err := fileWriter.Write(input.File); err != nil {
		return newBifrostOperationError("failed to write file data", err, providerName)
	}

	// Add model field
	if err := writer.WriteField("model", model); err != nil {
		return newBifrostOperationError("failed to write model field", err, providerName)
	}

	// Add optional fields
	if input.Language != nil {
		if err := writer.WriteField("language", *input.Language); err != nil {
			return newBifrostOperationError("failed to write language field", err, providerName)
		}
	}

	if input.Prompt != nil {
		if err := writer.WriteField("prompt", *input.Prompt); err != nil {
			return newBifrostOperationError("failed to write prompt field", err, providerName)
		}
	}

	if input.ResponseFormat != nil {
		if err := writer.WriteField("response_format", *input.ResponseFormat); err != nil {
			return newBifrostOperationError("failed to write response_format field", err, providerName)
		}
	}

	// Note: Temperature and TimestampGranularities can be added via params.ExtraParams if needed

	// Add extra params if provided
	if params != nil && params.ExtraParams != nil {
		for key, value := range params.ExtraParams {
			// Handle array parameters specially for OpenAI's form data format
			switch v := value.(type) {
			case []string:
				// For arrays like timestamp_granularities[] or include[]
				for _, item := range v {
					if err := writer.WriteField(key+"[]", item); err != nil {
						return newBifrostOperationError(fmt.Sprintf("failed to write array param %s", key), err, providerName)
					}
				}
			case []interface{}:
				// Handle generic interface arrays
				for _, item := range v {
					if err := writer.WriteField(key+"[]", fmt.Sprintf("%v", item)); err != nil {
						return newBifrostOperationError(fmt.Sprintf("failed to write array param %s", key), err, providerName)
					}
				}
			default:
				// Handle non-array parameters normally
				if err := writer.WriteField(key, fmt.Sprintf("%v", value)); err != nil {
					return newBifrostOperationError(fmt.Sprintf("failed to write extra param %s", key), err, providerName)
				}
			}
		}
	}

	// Close the multipart writer
	if err := writer.Close(); err != nil {
		return newBifrostOperationError("failed to close multipart writer", err, providerName)
	}

	return nil
}

func parseOpenAIError(resp *fasthttp.Response) *schemas.BifrostError {
	var errorResp schemas.BifrostError

	bifrostErr := handleProviderAPIError(resp, &errorResp)

	if errorResp.EventID != nil {
		bifrostErr.EventID = errorResp.EventID
	}
	bifrostErr.Error.Type = errorResp.Error.Type
	bifrostErr.Error.Code = errorResp.Error.Code
	bifrostErr.Error.Message = errorResp.Error.Message
	bifrostErr.Error.Param = errorResp.Error.Param
	if errorResp.Error.EventID != nil {
		bifrostErr.Error.EventID = errorResp.Error.EventID
	}

	return bifrostErr
}

func parseStreamOpenAIError(resp *http.Response) *schemas.BifrostError {
	var errorResp schemas.BifrostError

	statusCode := resp.StatusCode
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if err := sonic.Unmarshal(body, &errorResp); err != nil {
		return &schemas.BifrostError{
			IsBifrostError: true,
			StatusCode:     &statusCode,
			Error: schemas.ErrorField{
				Message: schemas.ErrProviderResponseUnmarshal,
				Error:   err,
			},
		}
	}

	bifrostErr := &schemas.BifrostError{
		IsBifrostError: false,
		StatusCode:     &statusCode,
		Error:          schemas.ErrorField{},
	}

	if errorResp.EventID != nil {
		bifrostErr.EventID = errorResp.EventID
	}
	bifrostErr.Error.Type = errorResp.Error.Type
	bifrostErr.Error.Code = errorResp.Error.Code
	bifrostErr.Error.Message = errorResp.Error.Message
	bifrostErr.Error.Param = errorResp.Error.Param
	if errorResp.Error.EventID != nil {
		bifrostErr.Error.EventID = errorResp.Error.EventID
	}

	return bifrostErr
}

func parseOpenAIErrorForStreamDataLine(jsonData string) (*schemas.BifrostError, error) {
	var openAIError schemas.BifrostError
	if err := sonic.Unmarshal([]byte(jsonData), &openAIError); err != nil {
		return nil, err
	}

	// Send error through channel
	bifrostErr := &schemas.BifrostError{
		IsBifrostError: false,
		Error: schemas.ErrorField{
			Type:    openAIError.Error.Type,
			Code:    openAIError.Error.Code,
			Message: openAIError.Error.Message,
			Param:   openAIError.Error.Param,
		},
	}

	if openAIError.EventID != nil {
		bifrostErr.EventID = openAIError.EventID
	}
	if openAIError.Error.EventID != nil {
		bifrostErr.Error.EventID = openAIError.Error.EventID
	}

	return bifrostErr, nil
}
