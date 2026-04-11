package database

import (
	"context"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"studymind-backend/internal/models"
)

const tableName = "StudyMind_Cache"

// CacheService wraps the DynamoDB client for caching operations.
type CacheService struct {
	client *dynamodb.Client
}

// cacheItem is the DynamoDB record schema.
type cacheItem struct {
	PK        string                `dynamodbav:"PK"`
	SK        string                `dynamodbav:"SK"`
	VideoURL  string                `dynamodbav:"video_url"`
	Title     string                `dynamodbav:"title"`
	Subtitles []models.SubtitleLine `dynamodbav:"subtitles"`
}

// NewCacheService creates a new DynamoDB-backed cache service.
func NewCacheService() (*CacheService, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS config: %w", err)
	}

	client := dynamodb.NewFromConfig(cfg)
	log.Println("[DynamoDB] Cache service initialized")
	return &CacheService{client: client}, nil
}

// GetCachedCourse looks up a cached translation result.
// Returns nil, nil if no cache entry exists.
func (c *CacheService) GetCachedCourse(videoID, targetLang string) (*models.ProcessResponse, error) {
	pk := fmt.Sprintf("VIDEO#%s", videoID)
	sk := fmt.Sprintf("LANG#%s", targetLang)

	result, err := c.client.GetItem(context.TODO(), &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("DynamoDB GetItem error: %w", err)
	}

	if result.Item == nil {
		return nil, nil // cache miss
	}

	var item cacheItem
	if err := attributevalue.UnmarshalMap(result.Item, &item); err != nil {
		return nil, fmt.Errorf("unmarshal cache item error: %w", err)
	}

	return &models.ProcessResponse{
		VideoID:    videoID,
		VideoURL:   item.VideoURL,
		TargetLang: targetLang,
		Title:      item.Title,
		Subtitles:  item.Subtitles,
		FromCache:  true,
	}, nil
}

// SaveToCache stores a translation result in DynamoDB.
func (c *CacheService) SaveToCache(videoID, targetLang string, resp *models.ProcessResponse) error {
	item := cacheItem{
		PK:        fmt.Sprintf("VIDEO#%s", videoID),
		SK:        fmt.Sprintf("LANG#%s", targetLang),
		VideoURL:  resp.VideoURL,
		Title:     resp.Title,
		Subtitles: resp.Subtitles,
	}

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("marshal cache item error: %w", err)
	}

	_, err = c.client.PutItem(context.TODO(), &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("DynamoDB PutItem error: %w", err)
	}

	log.Printf("[DynamoDB] Cached result for video=%s lang=%s", videoID, targetLang)
	return nil
}
