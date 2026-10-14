go test -v       

docker ps               # list running containers
docker stop redis       # stop the container
docker start redis      # start it again
docker logs redis       # see Redis's output/errors
docker exec -it redis redis-cli   # open the Redis shell again


go test -v -run TestRedisConnection