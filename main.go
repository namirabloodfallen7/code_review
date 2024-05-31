package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"log"
	"net/http"
	"sync"
)

// Написать мини сервис с разделением слоев в одном main.go файле. Можно писать в Goland
// Сервис должен уметь:

// Принимать http запросы REST like API
// Подключаться к базе данных
// Использовать кэш c применением Proxy паттерна
// Регистрировать пользователя в базе данных
// У пользователя следующие данные email, password, name, age
// Запретить регистрацию пользователей с одинаковым email и возрастом меньше 18 лет
// Вывести список всех пользователей

type User struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
	Age      int    `json:"age"`
}

//Слой кеша

type UserCache struct {
	users map[string]*User
	mutex sync.RWMutex
}

func NewUserCache() *UserCache {
	return &UserCache{
		users: make(map[string]*User),
	}
}

func (c *UserCache) Get(email string) (*User, bool) {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	user, found := c.users[email]
	return user, found
}

func (c *UserCache) Set(email string, user *User) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.users[email] = user
}

func (c *UserCache) List() []User {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	users := []User{}
	for _, user := range c.users {
		users = append(users, *user)
	}
	return users
}

type CachedUserRep struct {
	rep   UserRepository
	cache *UserCache
}

func NewCachedUserRep(rep UserRepository) *CachedUserRep {
	return &CachedUserRep{
		rep:   rep,
		cache: NewUserCache(),
	}
}

func (c *CachedUserRep) Create(ctx context.Context, user User) error {
	err := c.rep.Create(ctx, user)
	if err == nil {
		c.cache.Set(user.Email, &user)
	}
	return err
}

func (c *CachedUserRep) List(ctx context.Context) ([]User, error) {
	cachedUsers := c.cache.List()
	if len(cachedUsers) > 0 {
		return cachedUsers, nil
	}

	users, err := c.rep.List(ctx)
	if err == nil {
		for _, user := range users {
			c.cache.Set(user.Email, &user)
		}
	}
	return users, err
}

func (c *CachedUserRep) GetByEmail(email string) (*User, error) {
	user, found := c.cache.Get(email)
	if found {
		return user, nil
	}

	user, err := c.rep.GetByEmail(email)
	if err == sql.ErrNoRows {
		return nil, nil // Если пользователь не найден, возвращаем nil
	}
	if err == nil && user != nil {
		c.cache.Set(email, user)
	}
	return user, err
}

//Слой репозиторий __________________________________________________

type UserRepository interface {
	Create(ctx context.Context, user User) error
	List(ctx context.Context) ([]User, error)
	GetByEmail(email string) (*User, error)
}

type UserRep struct {
	db *sql.DB
}

func (u *UserRep) Create(ctx context.Context, user User) error {
	_, err := u.db.ExecContext(ctx, `INSERT INTO users (email, password, name, age) VALUES ($1, $2, $3, $4)`, user.Email, user.Password, user.Name, user.Age)
	return err
}

func (u *UserRep) List(ctx context.Context) ([]User, error) {
	rows, err := u.db.QueryContext(ctx, `SELECT email, password, name, age FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := []User{}
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.Email, &user.Password, &user.Name, &user.Age); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, nil
}

func (u *UserRep) GetByEmail(email string) (*User, error) {
	var user User
	row := u.db.QueryRow(`SELECT email, password, name, age FROM users WHERE email = $1`, email)
	err := row.Scan(&user.Email, &user.Password, &user.Name, &user.Age)
	if err == sql.ErrNoRows {
		return nil, nil // Если пользователь не найден, возвращаем nil
	}
	return &user, err
}

//Слой сервис __________________________________________________

type UserService interface {
	Create(ctx context.Context, user User) error
	List(ctx context.Context) ([]User, error)
}

type UserServ struct {
	rep UserRepository
}

func (u *UserServ) Create(ctx context.Context, user User) error {
	if user.Age < 18 {
		return errors.New("возраст пользователя меньше 18 лет")
	}

	existingUser, err := u.rep.GetByEmail(user.Email)
	if err != nil {
		return err
	}
	if existingUser != nil {
		return errors.New("пользователь с таким email уже зарегистрирован")
	}

	return u.rep.Create(ctx, user)
}

func (u *UserServ) List(ctx context.Context) ([]User, error) {
	return u.rep.List(ctx)
}

// cлой контролер ________________________________________

type UserController interface {
	Create(w http.ResponseWriter, r *http.Request)
	List(w http.ResponseWriter, r *http.Request)
}

type UserContr struct {
	serv UserService
}

func (u *UserContr) Create(w http.ResponseWriter, r *http.Request) {
	var user User
	if err := json.NewDecoder(r.Body).Decode(&user); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := u.serv.Create(r.Context(), user); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (u *UserContr) List(w http.ResponseWriter, r *http.Request) {
	users, err := u.serv.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(users); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func main() {
	db, err := InitDB("users.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	fmt.Println("База данных инициализирована")

	err = CreateTable(db)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Таблица users создана")

	userRep := &UserRep{db: db}
	cachedUserRep := NewCachedUserRep(userRep)
	userServ := &UserServ{rep: cachedUserRep}
	userContr := &UserContr{serv: userServ}

	http.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			userContr.Create(w, r)
		case http.MethodGet:
			userContr.List(w, r)
		default:
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})

	fmt.Println("Сервер запущен на порту :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// Создание бд и миграция _________________________________-

func InitDB(filepath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", filepath)
	if err != nil {
		return nil, err
	}
	return db, nil
}

func CreateTable(db *sql.DB) error {
	createTableSQL := `CREATE TABLE IF NOT EXISTS users (
		"id" INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
		"email" TEXT NOT NULL,
		"password" TEXT NOT NULL,
		"name" TEXT,
		"age" INTEGER
	);`

	_, err := db.Exec(createTableSQL)
	return err
}
