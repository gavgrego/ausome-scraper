package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
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

	// Create browser context
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserAgent(`Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36`),
		chromedp.DisableGPU,
		chromedp.NoDefaultBrowserCheck,
		chromedp.NoFirstRun,
		chromedp.Headless,
		chromedp.NoSandbox,
		chromedp.Flag("blink-settings", "scriptEnabled=false"),
	)

	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	// Create context without debug logging
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	// Run scraper in a loop
	for {
		if err := scrapeProducts(ctx, db); err != nil {
			log.Printf("Error in scrape cycle: %v", err)
		}
		time.Sleep(time.Minute * 5)
	}
}

func scrapeProducts(ctx context.Context, db *sql.DB) error {
	urls := []string{
		"https://www.costco.com/1-oz-gold-bar-rand-refinery-new-in-assay.product.4000202779.html",
	}

	log.Printf("Scraping %d products", len(urls))
	for _, url := range urls {
		// Navigate to page
		err := chromedp.Run(ctx,
			network.Enable(),
			chromedp.Navigate(url),
			chromedp.WaitVisible(`h1`, chromedp.ByQuery),
		)

		if err != nil {
			log.Printf("Failed to load page: %v", err)
			continue
		}

		log.Printf("Loaded page: %s", url)

		// Try to scrape product details
		var name, imageURL string
		var inStock bool

		err = chromedp.Run(ctx, chromedp.Tasks{
			// Get product name
			chromedp.EvaluateAsDevTools(`
                (() => {
                    const nameElement = document.querySelector('h1');
                    return nameElement ? nameElement.textContent.trim() : '';
                })()
            `, &name),

			// Try to get price
			// chromedp.EvaluateAsDevTools(`
			//           (() => {
			//               const selectors = [
			//                   'span[automation-id="productPriceOutput"]',
			//                   '.product-price',
			//                   '.your-price',
			//                   '.price',
			//                   'span[data-price]',
			//                   '[data-automation="product-price"]'
			//               ];

			//               for (const selector of selectors) {
			//                   const element = document.querySelector(selector);
			//                   if (element) {
			//                       return element.textContent.trim();
			//                   }
			//               }
			//               return '';
			//           })()
			//       `, &priceText),

			// Check stock status
			chromedp.EvaluateAsDevTools(`
                (() => {
                    const outOfStock = document.querySelector('[automation-id="outOfStockMessage"]') ||
                                     document.querySelector('.out-of-stock-msg') ||
                                     document.querySelector('.oos-overlay');
                    return !outOfStock;
                })()
            `, &inStock),

			// First wait for the image to be visible
			chromedp.WaitVisible(`#heroImage_zoom`, chromedp.ByID),

			// Get product image URL
			chromedp.EvaluateAsDevTools(`
				(() => {
					// Try to find any product image using multiple common selectors
					const selectors = [
						'img[alt*="Product"]',
						'img[alt*="product"]',
						'.MuiBox-root img',
						'[data-testid="Clickzoom_image_container"] img',
						'#zoomImg_wrapper img'
					];
					
					for (const selector of selectors) {
						const img = document.querySelector(selector);
						if (img) {
							console.log('Found image with selector:', selector);
							console.log('Image details:', {
								src: img.src,
								alt: img.alt,
								id: img.id
							});
							return img.src;
						}
					}
					
					console.log('No product image found with any selector');
					return '';
				})()
			`, &imageURL),
		})

		if err != nil {
			log.Printf("Failed to scrape details: %v", err)
			continue
		}

		// Only log the final scraped data
		log.Printf("Product: %s", name)
		// log.Printf("Price: %s", priceText)
		log.Printf("In Stock: %v", inStock)
		log.Printf("Image URL: %s", imageURL)

		// Parse price
		// var price float64
		// if priceText != "" {
		// 	fmt.Sscanf(priceText, "$%f", &price)
		// }

		// Update database
		product := Product{
			URL:     url,
			Name:    name,
			InStock: inStock,
			// Price:     price,
			ImageURL:  imageURL,
			UpdatedAt: time.Now(),
		}

		if err := updateProduct(db, product); err != nil {
			log.Printf("Database error: %v", err)
		} else {
			log.Printf("Successfully updated database")
		}

		time.Sleep(10 * time.Second)
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

// func startAPI(db *sql.DB) {
// 	// Add CORS middleware
// 	corsMiddleware := func(next http.HandlerFunc) http.HandlerFunc {
// 		return func(w http.ResponseWriter, r *http.Request) {
// 			w.Header().Set("Access-Control-Allow-Origin", "*")
// 			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
// 			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

// 			if r.Method == "OPTIONS" {
// 				w.WriteHeader(http.StatusOK)
// 				return
// 			}

// 			next(w, r)
// 		}
// 	}

// 	http.HandleFunc("/api/products", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
// 		if r.Method != http.MethodGet {
// 			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
// 			return
// 		}

// 		rows, err := db.Query("SELECT id, url, name, in_stock, price, image_url, updated_at FROM products")
// 		if err != nil {
// 			http.Error(w, err.Error(), http.StatusInternalServerError)
// 			return
// 		}
// 		defer rows.Close()

// 		var products []Product
// 		for rows.Next() {
// 			var p Product
// 			err := rows.Scan(&p.ID, &p.URL, &p.Name, &p.InStock, &p.Price, &p.ImageURL, &p.UpdatedAt)
// 			if err != nil {
// 				http.Error(w, err.Error(), http.StatusInternalServerError)
// 				return
// 			}
// 			products = append(products, p)
// 		}

// 		w.Header().Set("Content-Type", "application/json")
// 		json.NewEncoder(w).Encode(products)
// 	}))

// 	log.Printf("Starting API server on :8080")
// 	log.Fatal(http.ListenAndServe(":8080", nil))
// }
