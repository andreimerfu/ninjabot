package order

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rodrigo-brito/ninjabot/pkg/ent"
	"github.com/rodrigo-brito/ninjabot/pkg/ent/order"
	"github.com/rodrigo-brito/ninjabot/pkg/exchange"
	"github.com/rodrigo-brito/ninjabot/pkg/model"
	"github.com/rodrigo-brito/ninjabot/pkg/notification"

	"github.com/olekukonko/tablewriter"
	log "github.com/sirupsen/logrus"
)

type summary struct {
	Symbol string
	Win    int
	Lose   int
	Profit float64
}

func (s summary) String() string {
	tableString := &strings.Builder{}
	table := tablewriter.NewWriter(tableString)
	_, quote := exchange.SplitAssetQuote(s.Symbol)
	data := [][]string{
		{"Coin", s.Symbol},
		{"Trades", strconv.Itoa(s.Lose + s.Win)},
		{"Win", strconv.Itoa(s.Win)},
		{"Loss", strconv.Itoa(s.Lose)},
		{"% Win", fmt.Sprintf("%.1f", float64(s.Win)/float64(s.Win+s.Lose)*100)},
		{"Profit", fmt.Sprintf("%.4f %s", s.Profit, quote)},
	}
	table.AppendBulk(data)
	table.SetColumnAlignment([]int{tablewriter.ALIGN_LEFT, tablewriter.ALIGN_RIGHT})
	table.Render()
	return tableString.String()
}

type Controller struct {
	ctx       context.Context
	exchange  exchange.Exchange
	storage   *ent.Client
	orderFeed *Feed
	notifier  notification.Notifier
	Results   map[string]*summary
}

func NewController(ctx context.Context, exchange exchange.Exchange, storage *ent.Client,
	orderFeed *Feed, notifier notification.Notifier) *Controller {

	return &Controller{
		ctx:       ctx,
		storage:   storage,
		exchange:  exchange,
		orderFeed: orderFeed,
		notifier:  notifier,
		Results:   make(map[string]*summary),
	}
}

func (c *Controller) calculateProfit(o model.Order) (value, percent float64, err error) {
	orders, err := c.storage.Order.Query().Where(order.IDLT(o.ID)).All(c.ctx)
	if err != nil {
		return 0, 0, err
	}

	quantity := 0.0
	avgPrice := 0.0
	for _, order := range orders {
		if order.Side == string(model.SideTypeBuy) && order.Status == string(model.OrderStatusTypeFilled) {
			avgPrice = (order.Quantity*order.Price + avgPrice*quantity) / (order.Quantity + quantity)
			quantity += order.Quantity
		} else {
			quantity = math.Max(quantity-order.Quantity, 0)
		}
	}

	cost := o.Quantity * avgPrice
	profitValue := o.Quantity*o.Price - cost
	return profitValue, profitValue / cost, nil
}

func (c Controller) notify(message string) {
	log.Info(message)
	if c.notifier != nil {
		c.notifier.Notify(message)
	}
}

func (c *Controller) processTrade(order model.Order) {
	profitValue, profit, err := c.calculateProfit(order)
	if err != nil {
		log.Errorf("order/controller storage: %s", err)
	}
	if _, ok := c.Results[order.Symbol]; !ok {
		c.Results[order.Symbol] = &summary{Symbol: order.Symbol}
	}

	c.Results[order.Symbol].Profit += profitValue
	if profitValue > 0 {
		c.Results[order.Symbol].Win++
	} else {
		c.Results[order.Symbol].Lose++
	}

	_, quote := exchange.SplitAssetQuote(order.Symbol)
	c.notify(fmt.Sprintf("[PROFIT] %f %s (%f %%)\n%s", profitValue, quote, profit*100, c.Results[order.Symbol].String()))
}

func (c Controller) Start() {
	go func() {
		for range time.NewTicker(10 * time.Second).C {
			// get pending orders
			orders, err := c.storage.Order.Query().
				Where(order.StatusIn(
					string(model.OrderStatusTypeNew),
					string(model.OrderStatusTypePartiallyFilled),
					string(model.OrderStatusTypePendingCancel),
				)).
				Order(ent.Desc(order.FieldDate)).
				All(c.ctx)
			if err != nil {
				log.Error("orderController/start:", err)
				continue
			}

			// For each pending order, check for updates
			for _, order := range orders {
				excOrder, err := c.exchange.Order(order.Symbol, order.ExchangeID)
				if err != nil {
					log.WithField("id", order.ExchangeID).Error("orderControler/get: ", err)
					continue
				}

				// no status change
				if string(excOrder.Status) == order.Status {
					continue
				}

				excOrder.ID = order.ID

				_, err = order.Update().
					SetStatus(string(excOrder.Status)).
					SetQuantity(excOrder.Quantity).
					SetPrice(excOrder.Price).Save(c.ctx)
				if err != nil {
					log.Error("orderControler/update: ", err)
					continue
				}

				log.Infof("[ORDER %s] %s", excOrder.Status, excOrder)
				c.orderFeed.Publish(excOrder, false)
				if excOrder.Side == model.SideTypeSell {
					c.processTrade(excOrder)
				}
			}
		}
	}()
}

func (c Controller) createOrder(order *model.Order) error {
	register, err := c.storage.Order.Create().
		SetExchangeID(order.ExchangeID).
		SetDate(order.Date).
		SetPrice(order.Price).
		SetQuantity(order.Quantity).
		SetSide(string(order.Side)).
		SetSymbol(order.Symbol).
		SetType(string(order.Type)).
		SetStatus(string(order.Status)).
		SetNillableStop(order.Stop).
		SetNillableGroupID(order.GroupID).
		Save(c.ctx)
	if err != nil {
		return fmt.Errorf("error on save order: %w", err)
	}
	order.ID = register.ID
	go c.orderFeed.Publish(*order, true)
	return nil
}

func (c Controller) Account() (model.Account, error) {
	return c.exchange.Account()
}

func (c Controller) Position(symbol string) (asset, quote float64, err error) {
	return c.exchange.Position(symbol)
}

func (c Controller) Order(symbol string, id int64) (model.Order, error) {
	return c.exchange.Order(symbol, id)
}

func (c Controller) OrderOCO(side model.SideType, symbol string, size, price, stop,
	stopLimit float64) ([]model.Order, error) {

	log.Infof("[ORDER] Creating OCO order for %s", symbol)
	orders, err := c.exchange.OrderOCO(side, symbol, size, price, stop, stopLimit)
	if err != nil {
		log.Errorf("order/controller exchange: %s", err)
		return nil, err
	}

	for i := range orders {
		err := c.createOrder(&orders[i])
		if err != nil {
			log.Errorf("order/controller storage: %s", err)
			return nil, err
		}
	}

	return orders, nil
}

func (c Controller) OrderLimit(side model.SideType, symbol string, size, limit float64) (model.Order, error) {
	log.Infof("[ORDER] Creating LIMIT %s order for %s", side, symbol)
	order, err := c.exchange.OrderLimit(side, symbol, size, limit)
	if err != nil {
		log.Errorf("order/controller exchange: %s", err)
		return model.Order{}, err
	}

	err = c.createOrder(&order)
	if err != nil {
		log.Errorf("order/controller storage: %s", err)
		return model.Order{}, err
	}
	log.Infof("[ORDER CREATED] %s", order)
	return order, nil
}

func (c Controller) OrderMarket(side model.SideType, symbol string, size float64) (model.Order, error) {
	log.Infof("[ORDER] Creating MARKET %s order for %s", side, symbol)
	order, err := c.exchange.OrderMarket(side, symbol, size)
	if err != nil {
		log.Errorf("order/controller exchange: %s", err)
		return model.Order{}, err
	}

	err = c.createOrder(&order)
	if err != nil {
		log.Errorf("order/controller storage: %s", err)
		return model.Order{}, err
	}

	// calculate profit
	if order.Side == model.SideTypeSell {
		c.processTrade(order)
	}

	log.Infof("[ORDER CREATED] %s", order)
	return order, err
}

func (c Controller) Cancel(order model.Order) error {
	log.Infof("[ORDER] Cancelling order for %s", order.Symbol)
	err := c.exchange.Cancel(order)
	if err != nil {
		return err
	}

	_, err = c.storage.Order.UpdateOneID(order.ID).
		SetStatus(string(model.OrderStatusTypePendingCancel)).
		Save(c.ctx)
	if err != nil {
		log.Errorf("order/controller storage: %s", err)
		return err
	}
	log.Infof("[ORDER CANCELED] %s", order)
	return nil
}
