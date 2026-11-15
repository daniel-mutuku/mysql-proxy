FROM php:8.3-fpm-alpine

RUN docker-php-ext-install pdo_mysql

# Many FPM workers, so each one opening its own MySQL connection
# can exceed MySQL's max_connections (set to 20 in docker-compose.yml).
COPY www.conf /usr/local/etc/php-fpm.d/zz-pool.conf