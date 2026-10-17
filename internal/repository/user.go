package repository

import (
	"context"
	"errors"
	"fmt"
	"video-support-agent/internal/model"
	"video-support-agent/internal/service"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

type UserRepository struct {
	db *gorm.DB
}

func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{db: db}
}

func (r *UserRepository) Exists(ctx context.Context, id uint64) (bool, error) {
	var count int64

	err := r.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", id).Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("check user existence: %w", err)
	}

	return count > 0, nil
}

func (r *UserRepository) Create(ctx context.Context, user *model.User) error {
	err := r.db.WithContext(ctx).Create(user).Error
	if err == nil {
		return nil
	}

	if mysqlErr, ok := errors.AsType[*mysqlDriver.MySQLError](err); ok && mysqlErr.Number == 1062 {
		return service.ErrUsernameExists
	}

	return fmt.Errorf("create user: %w", err)
}

func (r *UserRepository) FindByUsername(ctx context.Context, username string) (*model.User, error) {
	var user model.User

	err := r.db.WithContext(ctx).Where("username = ?", username).First(&user).Error
	if err != nil {
		return nil, err
	}

	return &user, nil
}

func (r *UserRepository) FindByID(ctx context.Context, id uint64) (*model.User, error) {
	var user model.User

	err := r.db.WithContext(ctx).Where("id = ?", id).First(&user).Error
	if err != nil {
		return nil, fmt.Errorf("find user by id: %w", err)
	}

	return &user, nil
}
