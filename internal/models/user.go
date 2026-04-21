package models

type User struct {
	ID           string `json:"id" dynamodbav:"id"`
	Email        string `json:"email" dynamodbav:"email"`
	FirstName    string `json:"first_name" dynamodbav:"first_name"`
	LastName     string `json:"last_name" dynamodbav:"last_name"`
	University   string `json:"university,omitempty" dynamodbav:"university,omitempty"`
	Mobile       string `json:"mobile,omitempty" dynamodbav:"mobile,omitempty"`
	Country      string `json:"country,omitempty" dynamodbav:"country,omitempty"`
	DOB          string `json:"dob,omitempty" dynamodbav:"dob,omitempty"`
	PasswordHash string `json:"-" dynamodbav:"password_hash,omitempty"`
	GoogleSub    string `json:"-" dynamodbav:"google_sub,omitempty"`
	WechatOpenID string `json:"-" dynamodbav:"wechat_openid,omitempty"`
	Provider     string `json:"provider" dynamodbav:"provider"`
	CreatedAt    string `json:"created_at" dynamodbav:"created_at"`
}

type RegisterRequest struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	University string `json:"university"`
	Mobile    string `json:"mobile"`
	Country   string `json:"country"`
	DOB       string `json:"dob"`
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type AuthResponse struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}
