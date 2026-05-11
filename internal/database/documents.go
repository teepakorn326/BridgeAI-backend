package database

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"studymind-backend/internal/models"
)

// documentsTable is a separate DDB table from the lecture cache so the two
// pipelines stay cleanly isolated. Provision in AWS console with the same
// PK/SK schema as StudyMind_Cache.
const documentsTable = "StudyMind_Documents"

// documentItem is the DDB record for a translated document.
type documentItem struct {
	PK        string                 `dynamodbav:"PK"`
	SK        string                 `dynamodbav:"SK"`
	SourceURL string                 `dynamodbav:"source_url"`
	Source    string                 `dynamodbav:"source"`
	Title     string                 `dynamodbav:"title"`
	Blocks    []models.DocumentBlock `dynamodbav:"blocks"`
}

type userDocumentLink struct {
	PK         string `dynamodbav:"PK"`
	SK         string `dynamodbav:"SK"`
	DocumentID string `dynamodbav:"document_id"`
	SourceURL  string `dynamodbav:"source_url"`
	Source     string `dynamodbav:"source"`
	Title      string `dynamodbav:"title"`
	TargetLang string `dynamodbav:"target_lang"`
	CreatedAt  string `dynamodbav:"created_at"`
}

type documentStudyItem struct {
	PK      string `dynamodbav:"PK"`
	SK      string `dynamodbav:"SK"`
	Content string `dynamodbav:"content"`
}

// GetCachedDocument looks up a translated document by ID + target language.
// Returns nil, nil on cache miss.
func (c *CacheService) GetCachedDocument(documentID, targetLang string) (*models.Document, error) {
	pk := fmt.Sprintf("DOC#%s", documentID)
	sk := fmt.Sprintf("LANG#%s", targetLang)

	result, err := c.client.GetItem(context.TODO(), &dynamodb.GetItemInput{
		TableName: aws.String(documentsTable),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("DynamoDB GetItem (document) error: %w", err)
	}
	if result.Item == nil {
		return nil, nil
	}

	var item documentItem
	if err := attributevalue.UnmarshalMap(result.Item, &item); err != nil {
		return nil, fmt.Errorf("unmarshal document item error: %w", err)
	}

	return &models.Document{
		DocumentID: documentID,
		Source:     item.Source,
		SourceURL:  item.SourceURL,
		Title:      item.Title,
		TargetLang: targetLang,
		Blocks:     item.Blocks,
		FromCache:  true,
	}, nil
}

// SaveDocumentToCache stores a translated document.
func (c *CacheService) SaveDocumentToCache(doc *models.Document) error {
	item := documentItem{
		PK:        fmt.Sprintf("DOC#%s", doc.DocumentID),
		SK:        fmt.Sprintf("LANG#%s", doc.TargetLang),
		SourceURL: doc.SourceURL,
		Source:    doc.Source,
		Title:     doc.Title,
		Blocks:    doc.Blocks,
	}
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("marshal document item error: %w", err)
	}
	_, err = c.client.PutItem(context.TODO(), &dynamodb.PutItemInput{
		TableName: aws.String(documentsTable),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("DynamoDB PutItem (document) error: %w", err)
	}
	log.Printf("[DynamoDB] Cached document=%s lang=%s blocks=%d", doc.DocumentID, doc.TargetLang, len(doc.Blocks))
	return nil
}

// LinkUserToDocument records a (user, document, lang) tuple for the
// per-user document library. Idempotent.
func (c *CacheService) LinkUserToDocument(userID string, doc *models.Document) error {
	if userID == "" {
		return fmt.Errorf("userID required")
	}
	item := userDocumentLink{
		PK:         fmt.Sprintf("USER#%s", userID),
		SK:         fmt.Sprintf("DOC#%s#%s", doc.DocumentID, doc.TargetLang),
		DocumentID: doc.DocumentID,
		SourceURL:  doc.SourceURL,
		Source:     doc.Source,
		Title:      doc.Title,
		TargetLang: doc.TargetLang,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("marshal user-document link error: %w", err)
	}
	_, err = c.client.PutItem(context.TODO(), &dynamodb.PutItemInput{
		TableName: aws.String(documentsTable),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("DynamoDB PutItem (user-document) error: %w", err)
	}
	return nil
}

// ListUserDocuments returns the documents this user has uploaded.
func (c *CacheService) ListUserDocuments(userID string) ([]models.DocumentSummary, error) {
	if userID == "" {
		return nil, fmt.Errorf("userID required")
	}
	var summaries []models.DocumentSummary
	var lastKey map[string]types.AttributeValue

	for {
		out, err := c.client.Query(context.TODO(), &dynamodb.QueryInput{
			TableName:              aws.String(documentsTable),
			KeyConditionExpression: aws.String("PK = :pk AND begins_with(SK, :sk)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": &types.AttributeValueMemberS{Value: fmt.Sprintf("USER#%s", userID)},
				":sk": &types.AttributeValueMemberS{Value: "DOC#"},
			},
			ExclusiveStartKey: lastKey,
		})
		if err != nil {
			return nil, fmt.Errorf("DynamoDB Query (user docs) error: %w", err)
		}
		for _, raw := range out.Items {
			var item userDocumentLink
			if err := attributevalue.UnmarshalMap(raw, &item); err != nil {
				continue
			}
			summaries = append(summaries, models.DocumentSummary{
				DocumentID: item.DocumentID,
				SourceURL:  item.SourceURL,
				Source:     item.Source,
				Title:      item.Title,
				TargetLang: item.TargetLang,
			})
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		lastKey = out.LastEvaluatedKey
	}

	return summaries, nil
}

// GetDocumentStudyMaterial fetches a cached study material for a document.
// kind is one of: "summary", "quiz", "vocab".
func (c *CacheService) GetDocumentStudyMaterial(documentID, targetLang, kind string) (string, error) {
	pk := fmt.Sprintf("DOCSTUDY#%s#%s", kind, documentID)
	sk := fmt.Sprintf("LANG#%s", targetLang)

	result, err := c.client.GetItem(context.TODO(), &dynamodb.GetItemInput{
		TableName: aws.String(documentsTable),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
			"SK": &types.AttributeValueMemberS{Value: sk},
		},
	})
	if err != nil {
		return "", fmt.Errorf("DynamoDB GetItem (doc study) error: %w", err)
	}
	if result.Item == nil {
		return "", nil
	}
	var item documentStudyItem
	if err := attributevalue.UnmarshalMap(result.Item, &item); err != nil {
		return "", fmt.Errorf("unmarshal doc study error: %w", err)
	}
	return item.Content, nil
}

// SaveDocumentStudyMaterial stores an AI-generated study material for a document.
func (c *CacheService) SaveDocumentStudyMaterial(documentID, targetLang, kind, content string) error {
	item := documentStudyItem{
		PK:      fmt.Sprintf("DOCSTUDY#%s#%s", kind, documentID),
		SK:      fmt.Sprintf("LANG#%s", targetLang),
		Content: content,
	}
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("marshal doc study error: %w", err)
	}
	_, err = c.client.PutItem(context.TODO(), &dynamodb.PutItemInput{
		TableName: aws.String(documentsTable),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("DynamoDB PutItem (doc study) error: %w", err)
	}
	log.Printf("[DynamoDB] Cached doc-%s for document=%s lang=%s", kind, documentID, targetLang)
	return nil
}

