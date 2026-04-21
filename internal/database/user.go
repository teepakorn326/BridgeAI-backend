package database

import (
	"context"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"studymind-backend/internal/models"
)

const usersTable = "StudyMind_Users"

func (c *CacheService) PutUser(user *models.User) error {
	item, err := attributevalue.MarshalMap(user)
	if err != nil {
		return fmt.Errorf("marshal user: %w", err)
	}

	item["PK"] = &types.AttributeValueMemberS{Value: "USER#" + user.ID}
	item["SK"] = &types.AttributeValueMemberS{Value: "PROFILE"}

	_, err = c.client.PutItem(context.TODO(), &dynamodb.PutItemInput{
		TableName: aws.String(usersTable),
		Item:      item,
	})
	if err != nil {
		return fmt.Errorf("put user: %w", err)
	}
	log.Printf("[DynamoDB] User saved: %s", user.Email)
	return nil
}

func (c *CacheService) GetUserByID(id string) (*models.User, error) {
	result, err := c.client.GetItem(context.TODO(), &dynamodb.GetItemInput{
		TableName: aws.String(usersTable),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "USER#" + id},
			"SK": &types.AttributeValueMemberS{Value: "PROFILE"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	if result.Item == nil {
		return nil, nil
	}

	var user models.User
	if err := attributevalue.UnmarshalMap(result.Item, &user); err != nil {
		return nil, fmt.Errorf("unmarshal user: %w", err)
	}
	return &user, nil
}

func (c *CacheService) GetUserByEmail(email string) (*models.User, error) {
	result, err := c.client.Query(context.TODO(), &dynamodb.QueryInput{
		TableName:              aws.String(usersTable),
		IndexName:              aws.String("email-index"),
		KeyConditionExpression: aws.String("email = :email"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":email": &types.AttributeValueMemberS{Value: email},
		},
		Limit: aws.Int32(1),
	})
	if err != nil {
		return nil, fmt.Errorf("query user by email: %w", err)
	}
	if len(result.Items) == 0 {
		return nil, nil
	}

	var user models.User
	if err := attributevalue.UnmarshalMap(result.Items[0], &user); err != nil {
		return nil, fmt.Errorf("unmarshal user: %w", err)
	}
	return &user, nil
}

func (c *CacheService) GetUserByGoogleSub(sub string) (*models.User, error) {
	result, err := c.client.Query(context.TODO(), &dynamodb.QueryInput{
		TableName:              aws.String(usersTable),
		IndexName:              aws.String("google-sub-index"),
		KeyConditionExpression: aws.String("google_sub = :sub"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":sub": &types.AttributeValueMemberS{Value: sub},
		},
		Limit: aws.Int32(1),
	})
	if err != nil {
		return nil, fmt.Errorf("query user by google_sub: %w", err)
	}
	if len(result.Items) == 0 {
		return nil, nil
	}

	var user models.User
	if err := attributevalue.UnmarshalMap(result.Items[0], &user); err != nil {
		return nil, fmt.Errorf("unmarshal user: %w", err)
	}
	return &user, nil
}

func (c *CacheService) GetUserByWechatOpenID(openID string) (*models.User, error) {
	result, err := c.client.Query(context.TODO(), &dynamodb.QueryInput{
		TableName:              aws.String(usersTable),
		IndexName:              aws.String("wechat-openid-index"),
		KeyConditionExpression: aws.String("wechat_openid = :oid"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":oid": &types.AttributeValueMemberS{Value: openID},
		},
		Limit: aws.Int32(1),
	})
	if err != nil {
		return nil, fmt.Errorf("query user by wechat_openid: %w", err)
	}
	if len(result.Items) == 0 {
		return nil, nil
	}

	var user models.User
	if err := attributevalue.UnmarshalMap(result.Items[0], &user); err != nil {
		return nil, fmt.Errorf("unmarshal user: %w", err)
	}
	return &user, nil
}
