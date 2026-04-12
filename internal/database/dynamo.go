package database

import (
	"context"
	"fmt"
	"log"
	"strings"

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

// CourseSummary is a lightweight listing entry (no subtitles).
type CourseSummary struct {
	VideoID    string `json:"video_id"`
	VideoURL   string `json:"video_url"`
	Title      string `json:"title"`
	TargetLang string `json:"target_lang"`
}

// ListCachedCourses returns all cached video translations as summaries.
// Uses DynamoDB Scan with begins_with(PK, "VIDEO#") filter.
func (c *CacheService) ListCachedCourses() ([]CourseSummary, error) {
	var summaries []CourseSummary
	var lastKey map[string]types.AttributeValue

	for {
		out, err := c.client.Scan(context.TODO(), &dynamodb.ScanInput{
			TableName:                 aws.String(tableName),
			FilterExpression:          aws.String("begins_with(PK, :prefix)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":prefix": &types.AttributeValueMemberS{Value: "VIDEO#"}},
			ProjectionExpression:      aws.String("PK, SK, video_url, title"),
			ExclusiveStartKey:         lastKey,
		})
		if err != nil {
			return nil, fmt.Errorf("DynamoDB Scan error: %w", err)
		}

		for _, item := range out.Items {
			pk, _ := item["PK"].(*types.AttributeValueMemberS)
			sk, _ := item["SK"].(*types.AttributeValueMemberS)
			urlAttr, _ := item["video_url"].(*types.AttributeValueMemberS)
			titleAttr, _ := item["title"].(*types.AttributeValueMemberS)
			if pk == nil || sk == nil {
				continue
			}
			summaries = append(summaries, CourseSummary{
				VideoID:    strings.TrimPrefix(pk.Value, "VIDEO#"),
				TargetLang: strings.TrimPrefix(sk.Value, "LANG#"),
				VideoURL:   safeStr(urlAttr),
				Title:      safeStr(titleAttr),
			})
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		lastKey = out.LastEvaluatedKey
	}

	return summaries, nil
}

func safeStr(v *types.AttributeValueMemberS) string {
	if v == nil {
		return ""
	}
	return v.Value
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

// studyMaterialItem stores AI-generated study materials keyed by video+lang+kind.
type studyMaterialItem struct {
	PK      string `dynamodbav:"PK"`
	SK      string `dynamodbav:"SK"`
	Content string `dynamodbav:"content"` // JSON or markdown
}

// GetStudyMaterial fetches a cached study material (summary/quiz/vocab).
// kind is one of: "summary", "quiz", "vocab".
func (c *CacheService) GetStudyMaterial(videoID, targetLang, kind string) (string, error) {
	pk := fmt.Sprintf("STUDY#%s#%s", kind, videoID)
	sk := fmt.Sprintf("LANG#%s", targetLang)

	result, err := c.client.GetItem(context.TODO(), &dynamodb.GetItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil {
		return "", fmt.Errorf("DynamoDB GetItem error: %w", err)
	}
	if result.Item == nil {
		return "", nil
	}

	var item studyMaterialItem
	if err := attributevalue.UnmarshalMap(result.Item, &item); err != nil {
		return "", fmt.Errorf("unmarshal study material error: %w", err)
	}
	return item.Content, nil
}

// SaveStudyMaterial stores an AI-generated study material.
func (c *CacheService) SaveStudyMaterial(videoID, targetLang, kind, content string) error {
	item := studyMaterialItem{
		PK:      fmt.Sprintf("STUDY#%s#%s", kind, videoID),
		SK:      fmt.Sprintf("LANG#%s", targetLang),
		Content: content,
	}
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("marshal study material error: %w", err)
	}
	_, err = c.client.PutItem(context.TODO(), &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("DynamoDB PutItem error: %w", err)
	}
	log.Printf("[DynamoDB] Cached %s for video=%s lang=%s", kind, videoID, targetLang)
	return nil
}

// segmentsCacheItem stores translated segments keyed by content hash.
type segmentsCacheItem struct {
	PK       string                     `dynamodbav:"PK"`
	SK       string                     `dynamodbav:"SK"`
	Segments []models.TranslatedSegment `dynamodbav:"segments"`
}

// GetCachedSegments looks up translated segments by content hash.
func (c *CacheService) GetCachedSegments(hash, targetLang string) ([]models.TranslatedSegment, error) {
	pk := fmt.Sprintf("SEGMENTS#%s", hash)
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
		return nil, nil
	}

	var item segmentsCacheItem
	if err := attributevalue.UnmarshalMap(result.Item, &item); err != nil {
		return nil, fmt.Errorf("unmarshal segments cache error: %w", err)
	}
	return item.Segments, nil
}

// SaveSegmentsToCache stores translated segments by content hash.
func (c *CacheService) SaveSegmentsToCache(hash, targetLang string, segments []models.TranslatedSegment) error {
	item := segmentsCacheItem{
		PK:       fmt.Sprintf("SEGMENTS#%s", hash),
		SK:       fmt.Sprintf("LANG#%s", targetLang),
		Segments: segments,
	}
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("marshal segments cache error: %w", err)
	}
	_, err = c.client.PutItem(context.TODO(), &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("DynamoDB PutItem error: %w", err)
	}
	log.Printf("[DynamoDB] Cached %d segments for hash=%s lang=%s", len(segments), hash[:8], targetLang)
	return nil
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
