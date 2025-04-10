package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	speech "cloud.google.com/go/speech/apiv2"
	speechpb "cloud.google.com/go/speech/apiv2/speechpb"
	"google.golang.org/api/option"
)

type Config struct {
	ProjectID    string
	Region       string
	RecognizerID string
	PrimaryLang  string
	WAVInputPath string
	OneShot      bool
}

func loadConfig() (*Config, error) {
	// Parse command line flags
	primaryLang := flag.String("primary", "en-US", "Primary language code")
	wavInPath := flag.String("wav-in", "", "Path to read WAV file from")
	oneShot := flag.Bool("one-shot", false, "Use one-shot recognition instead of streaming")
	flag.Parse()

	config := &Config{
		ProjectID:    os.Getenv("GOOGLE_PROJECT_ID"),
		Region:       os.Getenv("GOOGLE_REGION"),
		RecognizerID: os.Getenv("RECOGNIZER_ID"),
		PrimaryLang:  *primaryLang,
		WAVInputPath: *wavInPath,
		OneShot:      *oneShot,
	}

	if config.ProjectID == "" {
		return nil, fmt.Errorf("GOOGLE_PROJECT_ID environment variable is not set")
	}

	if config.Region == "" {
		config.Region = "global"
		log.Printf("Missing GOOGLE_REGION environment variable, using %s", config.Region)
	}

	if config.RecognizerID == "" {
		return nil, fmt.Errorf("RECOGNIZER_ID environment variable is not set")
	}

	if config.WAVInputPath == "" {
		return nil, fmt.Errorf("WAV input path is not set")
	}

	return config, nil
}

type StreamingClient struct {
	client *speech.Client
	stream speechpb.Speech_StreamingRecognizeClient
}

func NewStreamingClient(ctx context.Context, config *Config) (*StreamingClient, error) {
	// Create client with explicit regional endpoint
	client, err := speech.NewClient(ctx,
		option.WithEndpoint(fmt.Sprintf("%s-speech.googleapis.com:443", config.Region)))
	if err != nil {
		return nil, fmt.Errorf("failed to create speech client: %w", err)
	}

	stream, err := client.StreamingRecognize(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create streaming client: %w", err)
	}

	// Send the initial configuration
	configReq := &speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_StreamingConfig{
			StreamingConfig: &speechpb.StreamingRecognitionConfig{
				Config: &speechpb.RecognitionConfig{
					DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{
						AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{},
					},
					LanguageCodes: []string{config.PrimaryLang},
					Model:         "latest_long",
				},
			},
		},
		Recognizer: fmt.Sprintf("projects/%s/locations/%s/recognizers/%s",
			config.ProjectID, config.Region, config.RecognizerID),
	}

	if err := stream.Send(configReq); err != nil {
		return nil, fmt.Errorf("failed to send config: %w", err)
	}

	return &StreamingClient{
		client: client,
		stream: stream,
	}, nil
}

func (c *StreamingClient) SendAudio(ctx context.Context, audio []byte) error {
	req := &speechpb.StreamingRecognizeRequest{
		StreamingRequest: &speechpb.StreamingRecognizeRequest_Audio{
			Audio: audio,
		},
	}
	log.Printf("Sending audio chunk: %d bytes", len(audio))
	return c.stream.Send(req)
}

func (c *StreamingClient) ReceiveTranscription(ctx context.Context) (*speechpb.StreamingRecognitionResult, error) {
	log.Printf("Waiting for transcription response...")
	resp, err := c.stream.Recv()
	if err != nil {
		if err == io.EOF {
			log.Printf("Stream ended with EOF")
			return nil, io.EOF
		}
		return nil, fmt.Errorf("failed to receive response: %w", err)
	}

	log.Printf("Received STT response: %+v", resp)

	if len(resp.Results) == 0 {
		log.Printf("No results in response")
		return nil, nil
	}

	return resp.Results[0], nil
}

func (c *StreamingClient) Close() error {
	if err := c.stream.CloseSend(); err != nil {
		return fmt.Errorf("failed to close stream: %w", err)
	}
	return c.client.Close()
}

func handleStreamingTranscription(ctx context.Context, config *Config, audioData []byte) error {
	// Streaming recognition
	client, err := NewStreamingClient(ctx, config)
	if err != nil {
		return fmt.Errorf("failed to create streaming client: %w", err)
	}
	defer client.Close()
	// Create error channel for goroutine error handling
	errChan := make(chan error, 2)

	// Send audio chunks in goroutine
	go func() {
		const chunkSize = 8192
		for i := 0; i < len(audioData); i += chunkSize {
			end := min(i+chunkSize, len(audioData))
			chunk := audioData[i:end]
			if err := client.SendAudio(ctx, chunk); err != nil {
				errChan <- fmt.Errorf("failed to send audio chunk: %w", err)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()

	// Receive transcriptions in goroutine
	go func() {
		for {
			result, err := client.ReceiveTranscription(ctx)
			if err != nil {
				if err == io.EOF {
					close(errChan)
					return
				}
				errChan <- fmt.Errorf("failed to receive transcription: %w", err)
				close(errChan)
				return
			}
			if result == nil {
				log.Printf("Received nil result")
				continue
			}
			if len(result.Alternatives) == 0 {
				log.Printf("Received empty alternatives")
				continue
			}
			alt := result.Alternatives[0]
			log.Printf("Transcription: %q (confidence: %.2f, final: %v)",
				alt.Transcript, alt.Confidence, result.IsFinal)
		}
	}()
	// Wait until errChan is closed to finish.
	for {
		select {
		case err := <-errChan:
			if err != nil {
				log.Fatalf("Error: %v", err)
			}
		default:
			if errChan == nil {
				return nil
			}
		}
	}
}

func handleOneShotTranscription(ctx context.Context, config *Config, audioData []byte) error {
	// One-shot recognition
	client, err := speech.NewClient(ctx,
		option.WithEndpoint(fmt.Sprintf("%s-speech.googleapis.com:443", config.Region)))
	if err != nil {
		return fmt.Errorf("failed to create speech client: %w", err)
	}
	defer client.Close()

	req := &speechpb.RecognizeRequest{
		Recognizer: fmt.Sprintf("projects/%s/locations/%s/recognizers/%s",
			config.ProjectID, config.Region, config.RecognizerID),
		Config: &speechpb.RecognitionConfig{
			DecodingConfig: &speechpb.RecognitionConfig_AutoDecodingConfig{
				AutoDecodingConfig: &speechpb.AutoDetectDecodingConfig{},
			},
			LanguageCodes: []string{config.PrimaryLang},
			Model:         "latest_long",
		},
		AudioSource: &speechpb.RecognizeRequest_Content{
			Content: audioData,
		},
	}

	log.Printf("Sending one-shot recognition request...")
	resp, err := client.Recognize(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to recognize audio: %w", err)
	}

	if len(resp.Results) == 0 {
		return fmt.Errorf("no results in response")
	}

	result := resp.Results[0]
	if len(result.Alternatives) == 0 {
		return fmt.Errorf("no alternatives in result")
	}

	alt := result.Alternatives[0]
	log.Printf("One-shot recognition succeeded: %q (confidence: %.2f)",
		alt.Transcript, alt.Confidence)
	return nil
}

func main() {
	// Load configuration
	config, err := loadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	fmt.Printf("Configuration: %+v\n", config)

	// Create context
	ctx := context.Background()

	// Read WAV file
	log.Printf("Reading WAV file from %s", config.WAVInputPath)
	audioData, err := os.ReadFile(config.WAVInputPath)
	if err != nil {
		log.Fatalf("failed to read WAV file: %w", err)
	}

	// Handle WAV input
	if config.OneShot {
		if err := handleOneShotTranscription(ctx, config, audioData); err != nil {
			log.Fatalf("Failed to handle one-shot WAV input: %v", err)
		}
		return
	} else {
		if err := handleStreamingTranscription(ctx, config, audioData); err != nil {
			log.Fatalf("Failed to handle streaming WAV input: %v", err)
		}
	}
}
