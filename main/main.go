package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

type Product struct {
	ID        int       `json:"id"`
	URL       string    `json:"url"`
	Name      string    `json:"name"`
	InStock   bool      `json:"in_stock"`
	Price     float64   `json:"price"`
	ImageURL  string    `json:"image_url"`
	UpdatedAt time.Time `json:"updated_at"`
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Fatal("Error loading .env file")
	}

	host := os.Getenv("DB_HOST")
	port := os.Getenv("DB_PORT")
	user := os.Getenv("DB_USER")
	password := os.Getenv("DB_PASSWORD")
	dbname := os.Getenv("DB_NAME")

	// Set up logging to both file and stdout
	logFile, err := os.OpenFile("scraper.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("error opening file: %v", err)
	}
	defer logFile.Close()

	// Create a multi writer to write to both file and stdout
	mw := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(mw)

	// Only log essential information
	log.SetFlags(log.Ltime | log.Ldate)

	// Initialize database connection
	psqlInfo := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=require",
		host, port, user, password, dbname)

	db, err := sql.Open("postgres", psqlInfo)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Create browser context with additional anti-detection flags
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserAgent(`Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36`),
		chromedp.DisableGPU,
		chromedp.NoDefaultBrowserCheck,
		chromedp.NoFirstRun,
		// The following flag can help obscure headless behavior
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		// For testing, you might temporarily remove headless mode:
		chromedp.Headless,
		chromedp.NoSandbox,
		chromedp.Flag("blink-settings", "scriptEnabled=false, imagesEnabled=false"),
	)

	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	// Create context without debug logging
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	go startAPI(db)

	// Run scraper in a loop
	for {
		if err := scrapeProducts(ctx, db); err != nil {
			log.Printf("Error in scrape cycle: %v", err)
		}
		// time.Sleep(time.Minute * 5)
	}

}

func scrapeProducts(ctx context.Context, db *sql.DB) error {
	// Read the file contents
	data, err := os.ReadFile("urls.json")
	if err != nil {
		log.Printf("Error reading urls.json: %v", err)
		return err
	}

	// Unmarshal the JSON into a []string slice
	var urls []string
	if err := json.Unmarshal(data, &urls); err != nil {
		log.Printf("Error parsing urls.json: %v", err)
		return err
	}

	log.Printf("Scraping %d products", len(urls))

	// Set up flags for each new Chrome process to further obfuscate scraping activity.
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserAgent(`Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36`),
		chromedp.DisableGPU,
		chromedp.NoDefaultBrowserCheck,
		chromedp.NoFirstRun,
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		// For debugging, you can disable headless mode:
		// chromedp.Headful,
		chromedp.Headless,
		chromedp.NoSandbox,
		chromedp.Flag("blink-settings", "scriptEnabled=false, imagesEnabled=false"),
		chromedp.Flag("ignore-certificate-errors", "true"),
		chromedp.Flag("disable-http2", "true"), // Experimental flag – may or may not help.
		chromedp.Flag("disable-extensions", "true"),
	)

	for _, url := range urls {
		// Optional randomized delay before launching a new browser process.
		time.Sleep(time.Duration(2+rand.Intn(5)) * time.Second)

		// Create a fresh ExecAllocator and derived context for each URL.
		allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
		tbCtx, cancel := chromedp.NewContext(allocCtx)
		// Increase the timeout to 60 seconds to allow slower pages to load.
		tbCtx, timeoutCancel := context.WithTimeout(tbCtx, 60*time.Second)

		var name, imageURL string
		var inStock bool

		log.Printf("Navigating to: %s", url)

		err := chromedp.Run(tbCtx,
			network.Enable(),
			chromedp.Navigate(url),
			// Wait for the <body> to be ready.
			chromedp.WaitReady("body"),
			// Wait for a specific element that signifies the product is loaded.
			// If this selector never appears (maybe due to a CAPTCHA or error), it will timeout.
			chromedp.WaitVisible(`h1[automation-id="productName"], .product-title, h1`, chromedp.ByQuery),
			// Evaluate the product name.
			chromedp.EvaluateAsDevTools(`
				(() => {
					const nameElement = document.querySelector('h1[automation-id="productName"]') ||
										  document.querySelector('.product-title') ||
										  document.querySelector('h1');
					return nameElement ? nameElement.textContent.trim() : '';
				})()
			`, &name),
			// Evaluate if the product is in stock.
			chromedp.EvaluateAsDevTools(`
				(() => {
					const outOfStock = document.querySelector('[automation-id="outOfStockMessage"]') ||
										 document.querySelector('.out-of-stock-msg') ||
										 document.querySelector('.oos-overlay');
					return !outOfStock;
				})()
			`, &inStock),
		)
		timeoutCancel()
		cancel()
		allocCancel()

		// If we encountered an error, try to capture a screenshot for debugging.
		if err != nil {
			allocCtx, allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
			tbCtx, cancel = chromedp.NewContext(allocCtx)
			tbCtx, capTimeout := context.WithTimeout(tbCtx, 10*time.Second)
			var buf []byte
			if errScr := chromedp.Run(tbCtx, chromedp.FullScreenshot(&buf, 90)); errScr == nil {
				filename := fmt.Sprintf("screenshots/screenshot_%d.png", time.Now().UnixNano())
				if errWrite := os.WriteFile(filename, buf, 0644); errWrite == nil {
					log.Printf("Saved screenshot for %s as %s", url, filename)
				} else {
					log.Printf("Failed to write screenshot: %v", errWrite)
				}
			}
			capTimeout()
			cancel()
			allocCancel()

			log.Printf("Failed to load or scrape page %s: %v", url, err)
			continue
		}

		log.Printf("Product: %s", name)
		log.Printf("In Stock: %v", inStock)

		// Update the database with scraped data.
		product := Product{
			URL:       url,
			Name:      name,
			InStock:   inStock,
			ImageURL:  imageURL,
			UpdatedAt: time.Now(),
		}

		if err := updateProduct(db, product); err != nil {
			log.Printf("Database error: %v", err)
		} else {
			log.Printf("Successfully updated database")
		}

		// Introduce a random delay after each scrape.
		time.Sleep(time.Duration(5+rand.Intn(10)) * time.Second)
	}

	return nil
}

func updateProduct(db *sql.DB, product Product) error {
	query := `
        INSERT INTO products (url, name, in_stock, price, image_url, updated_at)
        VALUES ($1, $2, $3, $4, $5, $6)
        ON CONFLICT (url) DO UPDATE
        SET name = $2, in_stock = $3, price = $4, image_url = $5, updated_at = $6
    `

	_, err := db.Exec(query, product.URL, product.Name, product.InStock, product.Price, product.ImageURL, product.UpdatedAt)
	return err
}

func startAPI(db *sql.DB) {
	// Add CORS middleware
	corsMiddleware := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			next(w, r)
		}
	}

	http.HandleFunc("/api/products", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		rows, err := db.Query("SELECT id, url, name, in_stock, price, image_url, updated_at FROM products")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var products []Product
		for rows.Next() {
			var p Product
			err := rows.Scan(&p.ID, &p.URL, &p.Name, &p.InStock, &p.Price, &p.ImageURL, &p.UpdatedAt)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			products = append(products, p)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(products)
	}))

	log.Printf("Starting API server on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
