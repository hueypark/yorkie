package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"github.com/hackerwins/yorkie/pkg/document/change"
	"github.com/hackerwins/yorkie/pkg/log"
	"github.com/hackerwins/yorkie/yorkie/types"
)

var (
	ErrClientNotFound   = errors.New("fail to find the client")
	ErrDocumentNotFound = errors.New("fail to find the document")
)

type Config struct {
	ConnectionTimeoutSec time.Duration `json:"ConnectionTimeOutSec"`
	ConnectionURI        string        `json:"ConnectionURI"`
	YorkieDatabase       string        `json:"YorkieDatabase"`
	PingTimeoutSec       time.Duration `json:"PingTimeoutSec"`
}

type Client struct {
	config *Config
	client *mongo.Client
}

func NewClient(conf *Config) (*Client, error) {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		conf.ConnectionTimeoutSec*time.Second,
	)
	defer cancel()

	client, err := mongo.Connect(
		ctx,
		options.Client().ApplyURI(conf.ConnectionURI),
	)
	if err != nil {
		log.Logger.Error(err)
		return nil, err
	}

	ctxPing, cancel := context.WithTimeout(ctx, conf.PingTimeoutSec*time.Second)
	defer cancel()

	if err := client.Ping(ctxPing, readpref.Primary()); err != nil {
		log.Logger.Error(err)
		return nil, err
	}

	if err := ensureIndex(ctx, client.Database(conf.YorkieDatabase)); err != nil {
		log.Logger.Error(err)
		return nil, err
	}

	log.Logger.Infof("connected, URI: %s, DB: %s", conf.ConnectionURI, conf.YorkieDatabase)

	return &Client{
		config: conf,
		client: client,
	}, nil
}

func (c *Client) Close() error {
	if err := c.client.Disconnect(context.Background()); err != nil {
		log.Logger.Error(err)
		return err
	}

	return nil
}

func (c *Client) ActivateClient(ctx context.Context, key string) (*types.ClientInfo, error) {
	clientInfo := types.ClientInfo{}
	if err := c.withCollection(ColClientInfos, func(col *mongo.Collection) error {
		now := time.Now()
		res, err := col.UpdateOne(ctx, bson.M{
			"key": key,
		}, bson.M{
			"$set": bson.M{
				"status":     types.ClientActivated,
				"updated_at": now,
			},
		}, options.Update().SetUpsert(true))
		if err != nil {
			log.Logger.Error(err)
			return err
		}

		var result *mongo.SingleResult
		if res.UpsertedCount > 0 {
			result = col.FindOneAndUpdate(ctx, bson.M{
				"_id": res.UpsertedID,
			}, bson.M{
				"$set": bson.M{
					"created_at": now,
				},
			})
		} else {
			result = col.FindOne(ctx, bson.M{
				"key": key,
			})
		}

		if err := result.Decode(&clientInfo); err != nil {
			log.Logger.Error(err)
			return err
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return &clientInfo, nil
}

func (c *Client) DeactivateClient(ctx context.Context, clientID string) (*types.ClientInfo, error) {
	clientInfo := types.ClientInfo{}
	if err := c.withCollection(ColClientInfos, func(col *mongo.Collection) error {
		id, err := primitive.ObjectIDFromHex(clientID)
		if err != nil {
			log.Logger.Error(err)
			return err
		}
		res := col.FindOneAndUpdate(ctx, bson.M{
			"_id": id,
		}, bson.M{
			"$set": bson.M{
				"status":     types.ClientDeactivated,
				"updated_at": time.Now(),
			},
		})

		if err := res.Decode(&clientInfo); err != nil {
			if err == mongo.ErrNoDocuments {
				return ErrClientNotFound
			}

			log.Logger.Error(err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &clientInfo, nil
}

func (c *Client) FindClientInfoByID(ctx context.Context, clientID string) (*types.ClientInfo, error) {
	var client types.ClientInfo

	if err := c.withCollection(ColClientInfos, func(col *mongo.Collection) error {
		id, err := primitive.ObjectIDFromHex(clientID)
		if err != nil {
			log.Logger.Error(err)
			return err
		}
		result := col.FindOne(ctx, bson.M{
			"_id": id,
		})

		if err := result.Decode(&client); err != nil {
			if err == mongo.ErrNoDocuments {
				return ErrClientNotFound
			}
			log.Logger.Error(err)
			return err
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return &client, nil
}

func (c *Client) UpdateClientInfoAfterPushPull(
	ctx context.Context,
	clientInfo *types.ClientInfo,
	docInfo *types.DocInfo,
) error {
	return c.withCollection(ColClientInfos, func(col *mongo.Collection) error {
		result := col.FindOneAndUpdate(ctx, bson.M{
			"key": clientInfo.Key,
		}, bson.M{
			"$set": bson.M{
				"documents." + docInfo.ID.Hex(): clientInfo.Documents[docInfo.ID.Hex()],
				"updated_at":                    clientInfo.UpdatedAt,
			},
		})

		if result.Err() != nil {
			if result.Err() == mongo.ErrNoDocuments {
				return ErrClientNotFound
			}
			log.Logger.Error(result.Err())
			return result.Err()
		}

		return nil
	})
}

func (c *Client) FindDocInfoByKey(
	ctx context.Context,
	clientInfo *types.ClientInfo,
	bsonDocKey string,
) (*types.DocInfo, error) {
	docInfo := types.DocInfo{}

	if err := c.withCollection(ColDocInfos, func(col *mongo.Collection) error {
		now := time.Now()
		res, err := col.UpdateOne(ctx, bson.M{
			"key": bsonDocKey,
		}, bson.M{
			"$set": bson.M{
				"accessed_at": now,
			},
		}, options.Update().SetUpsert(true))
		if err != nil {
			log.Logger.Error(err)
			return err
		}

		var result *mongo.SingleResult
		if res.UpsertedCount > 0 {
			result = col.FindOneAndUpdate(ctx, bson.M{
				"_id": res.UpsertedID,
			}, bson.M{
				"$set": bson.M{
					"owner":      clientInfo.ID,
					"created_at": now,
				},
			})
		} else {
			result = col.FindOne(ctx, bson.M{
				"key": bsonDocKey,
			})
		}

		if err := result.Decode(&docInfo); err != nil {
			log.Logger.Error(err)
			return err
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return &docInfo, nil
}

func (c *Client) CreateChangeInfos(
	ctx context.Context,
	docID primitive.ObjectID,
	changes []*change.Change,
) error {
	if len(changes) == 0 {
		return nil
	}

	return c.withCollection(ColChanges, func(col *mongo.Collection) error {
		var bsonChanges []interface{}

		for _, c := range changes {
			bsonChanges = append(bsonChanges, bson.M{
				"doc_id":     docID,
				"actor":      types.EncodeActorID(c.ID().Actor()),
				"server_seq": c.ServerSeq(),
				"client_seq": c.ID().ClientSeq(),
				"lamport":    c.ID().Lamport(),
				"message":    c.Message(),
				"operations": types.EncodeOperation(c.Operations()),
			})
		}

		_, err := col.InsertMany(ctx, bsonChanges, options.InsertMany().SetOrdered(true))
		if err != nil {
			log.Logger.Error(err)
			return err
		}

		return nil
	})
}

func (c *Client) UpdateDocInfo(
	ctx context.Context,
	clientInfo *types.ClientInfo,
	docInfo *types.DocInfo,
) error {
	return c.withCollection(ColDocInfos, func(col *mongo.Collection) error {
		now := time.Now()
		_, err := col.UpdateOne(ctx, bson.M{
			"_id": docInfo.ID,
		}, bson.M{
			"$set": bson.M{
				"server_seq": docInfo.ServerSeq,
				"updated_at": now,
			},
		})

		if err != nil {
			if err == mongo.ErrNoDocuments {
				return ErrDocumentNotFound
			}

			log.Logger.Error(err)
			return err
		}

		return nil
	})
}

func (c *Client) FindChangeInfosBetweenServerSeqs(
	ctx context.Context,
	docID primitive.ObjectID,
	from uint64,
	to uint64,
) ([]*change.Change, error) {
	var changes []*change.Change

	if err := c.withCollection(ColChanges, func(col *mongo.Collection) error {
		cursor, err := col.Find(ctx, bson.M{
			"doc_id": docID,
			"server_seq": bson.M{
				"$gte": from,
				"$lte": to,
			},
		}, options.Find())
		if err != nil {
			log.Logger.Error(err)
			return err
		}

		defer func() {
			if err := cursor.Close(ctx); err != nil {
				log.Logger.Error(err)
			}
		}()

		for cursor.Next(ctx) {
			var changeInfo types.ChangeInfo
			if err := cursor.Decode(&changeInfo); err != nil {
				log.Logger.Error(err)
				return err
			}

			c, err := changeInfo.ToChange()
			if err != nil {
				return err
			}
			changes = append(changes, c)
		}

		if cursor.Err() != nil {
			log.Logger.Error(cursor.Err())
			return cursor.Err()
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return changes, nil
}

func (c *Client) withCollection(
	collection string,
	callback func(collection *mongo.Collection) error,
) error {
	col := c.client.Database(c.config.YorkieDatabase).Collection(collection)
	if err := callback(col); err != nil {
		return err
	}

	return nil
}
